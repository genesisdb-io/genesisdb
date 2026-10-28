package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// Index format constants. The shard count is part of the format: changing it
// requires an explicit migration and a new format version.
const (
	FormatName = "genesisdb-sharded-index"
	ShardCount = 256

	// MaxIndexFiles is the maximum number of index files: five sharded
	// categories, the subject catalog and the state journal.
	MaxIndexFiles = 5*ShardCount + 2

	// maxRecordLength bounds a record length accepted from index data.
	maxRecordLength = int64(1) << 31
)

// Index categories. The names are directory names on disk.
const (
	categoryEventsBySubject              = "events-by-subject"
	categoryEventsBySubjectWithAncestors = "events-by-subject-recursive"
	categoryEventTypes                   = "event-types"
	categoryRetiredLatestEvent           = "latest-event"
	categoryLatestEventOfSubjectAndType  = "latest-event-of-type"
	categoryEventsByID                   = "events-by-id"
	categorySubjectCatalog               = "subjects"
	categoryIndexState                   = "index-state"
)

// shardedCategories lists every hash-sharded category in a fixed order.
var shardedCategories = []string{
	categoryEventsBySubject,
	categoryEventsBySubjectWithAncestors,
	categoryEventTypes,
	categoryLatestEventOfSubjectAndType,
	categoryEventsByID,
}

// categoriesWithDisposableData lists every category directory that can hold
// data of the pre 1.1.6 layout, of the retired latest-event category, or of
// the other storage format.
var categoriesWithDisposableData = []string{
	categorySubjectCatalog,
	categoryEventsBySubject,
	categoryEventsBySubjectWithAncestors,
	categoryEventTypes,
	categoryRetiredLatestEvent,
	categoryLatestEventOfSubjectAndType,
	categoryEventsByID,
}

// categoryDirectories lists every category directory of the current layout.
var categoryDirectories = []string{
	categorySubjectCatalog,
	categoryEventsBySubject,
	categoryEventsBySubjectWithAncestors,
	categoryEventTypes,
	categoryLatestEventOfSubjectAndType,
	categoryEventsByID,
}

// shardNumberForKey routes a raw UTF-8 key to a shard: SHA-256(key)[0].
func shardNumberForKey(key string) int {
	sum := sha256.Sum256([]byte(key))
	return int(sum[0])
}

// subjectAndTypeRoutingKey encodes a subject and type pair unambiguously with
// 4 byte big-endian length prefixes.
func subjectAndTypeRoutingKey(subject, eventType string) []byte {
	buffer := make([]byte, 0, 8+len(subject)+len(eventType))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(subject)))
	buffer = append(buffer, length[:]...)
	buffer = append(buffer, subject...)
	binary.BigEndian.PutUint32(length[:], uint32(len(eventType)))
	buffer = append(buffer, length[:]...)
	buffer = append(buffer, eventType...)
	return buffer
}

// shardNumberForSubjectAndType routes a subject and type pair to a shard.
func shardNumberForSubjectAndType(subject, eventType string) int {
	sum := sha256.Sum256(subjectAndTypeRoutingKey(subject, eventType))
	return int(sum[0])
}

// paths builds the relative paths of index files for one storage format.
type paths struct {
	format layout.Format
}

func (p paths) shard(category string, shard int) string {
	return category + "/" + p.format.ShardFileName(shard)
}

func (p paths) catalog() string {
	return categorySubjectCatalog + "/" + p.format.IndexCatalogFile()
}

// categoryOf returns category and shard (-1 for the catalog) of a relative
// path.
func (p paths) categoryOf(relativePath string) (string, int, bool) {
	if relativePath == p.catalog() {
		return categorySubjectCatalog, -1, true
	}
	separator := strings.IndexByte(relativePath, '/')
	if separator < 0 {
		return "", 0, false
	}
	category, name := relativePath[:separator], relativePath[separator+1:]
	shard, ok := p.format.ParseShardFileName(name)
	if !ok {
		return "", 0, false
	}
	for _, sharded := range shardedCategories {
		if sharded == category {
			return category, shard, true
		}
	}
	return "", 0, false
}

// all returns every possible index file path except the state journal.
func (p paths) all() []string {
	result := make([]string, 0, MaxIndexFiles-1)
	for _, category := range shardedCategories {
		for shard := 0; shard < ShardCount; shard++ {
			result = append(result, p.shard(category, shard))
		}
	}
	return append(result, p.catalog())
}

// ShardHeader is the first line of every index file.
type ShardHeader struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	Category   string `json:"category"`
	Shard      string `json:"shard,omitempty"`
	ShardCount int    `json:"shardCount"`
}

// encodeShardHeader returns the header line with newline. A negative shard
// marks a file that is not sharded (catalog or state journal). The version
// is the record version of the file.
func encodeShardHeader(version int, category string, shard int) []byte {
	header := ShardHeader{Format: FormatName, Version: version, Category: category, ShardCount: ShardCount}
	if shard >= 0 {
		header.Shard = fmt.Sprintf("%02x", shard)
	}
	encoded, _ := json.Marshal(header)
	return append(encoded, '\n')
}

// shardHeaderKind classifies the first line of an index file.
type shardHeaderKind int

const (
	shardHeaderEmpty shardHeaderKind = iota
	shardHeaderLegacy
	// shardHeaderCurrent is a header of a known record version. Whether it is
	// the version the index writes is decided by the caller.
	shardHeaderCurrent
	shardHeaderFuture
	shardHeaderInvalid
)

func classifyShardHeader(line []byte) (shardHeaderKind, *ShardHeader) {
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return shardHeaderEmpty, nil
	}
	if string(line) == layout.FormatLegacy.LogHeader() {
		return shardHeaderLegacy, nil
	}
	var header ShardHeader
	if err := json.Unmarshal(line, &header); err != nil || header.Format != FormatName {
		return shardHeaderInvalid, nil
	}
	if header.Version > newestRecordVersion {
		return shardHeaderFuture, &header
	}
	if header.Version < jsonRecordVersion {
		return shardHeaderInvalid, &header
	}
	if header.ShardCount != ShardCount {
		return shardHeaderFuture, &header
	}
	return shardHeaderCurrent, &header
}

// validateShardHeader checks the header of a file written by this index:
// known format, the given record version, category and shard. A file of
// another known record version is reported with errOtherRecordVersion, so
// the caller rebuilds instead of reading it with the wrong codec.
func validateShardHeader(line []byte, version int, category string, shard int) error {
	kind, header := classifyShardHeader(line)
	switch kind {
	case shardHeaderCurrent:
	case shardHeaderFuture:
		return fmt.Errorf("%w: version %d shardCount %d", core.ErrUnsupportedIndexFormat, header.Version, header.ShardCount)
	default:
		return newCorruptionError("invalid header for %s", category)
	}
	if header.Version != version {
		return fmt.Errorf("%w: %s has record version %d, expected %d", errOtherRecordVersion, category, header.Version, version)
	}
	if header.Category != category {
		return newCorruptionError("header category %q, expected %q", header.Category, category)
	}
	expectedShard := ""
	if shard >= 0 {
		expectedShard = fmt.Sprintf("%02x", shard)
	}
	if header.Shard != expectedShard {
		return newCorruptionError("header shard %q, expected %q", header.Shard, expectedShard)
	}
	return nil
}

// errOtherRecordVersion reports index data of a known but different record
// version, for example version 2 written by 1.1.6 in a directory that now
// uses version 3. It is derived data, the index is rebuilt.
var errOtherRecordVersion = errors.New("index records of another version")

// errInvalidRecord reports an index record that does not decode.
var errInvalidRecord = errors.New("invalid index record")

func newCorruptionError(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", core.ErrIndexCorrupt, fmt.Sprintf(format, arguments...))
}
