package index

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// Index record versions. A shard header names the version of its records.
const (
	// jsonRecordVersion is the record format of GenesisDB 1.1.6: one JSON
	// object per line. Directories in the legacy storage format keep it, so
	// they stay readable by 1.x.
	jsonRecordVersion = 2
	// compactRecordVersion is the record format of the current storage
	// format: tab separated fields without JSON, see compactCodec.
	compactRecordVersion = 3
	// newestRecordVersion is the newest version this binary understands.
	// A higher version is refused instead of rebuilt.
	newestRecordVersion = compactRecordVersion
)

// postingCodec encodes and decodes index records of one record version.
// Every record is one line, so framing, published boundaries, truncation and
// recovery do not depend on the codec.
type postingCodec interface {
	version() int
	encode(posting *Posting) []byte
	decode(line []byte, posting *Posting) error
	// keysFilter accepts exactly the lines whose key is one of keys.
	keysFilter(keys ...string) LineFilter
	// pairFilter accepts exactly the lines of the subject and type pair
	// category with the given subject and type.
	pairFilter(subject, eventType string) LineFilter
}

// codecForFormat returns the record codec of a storage format.
func codecForFormat(format layout.Format) postingCodec {
	if format == layout.FormatLegacy {
		return jsonCodec{}
	}
	return compactCodec{}
}

// jsonCodec is the record format of 1.1.6.
type jsonCodec struct{}

func (jsonCodec) version() int { return jsonRecordVersion }

func (jsonCodec) encode(posting *Posting) []byte { return encodePosting(posting) }

func (jsonCodec) decode(line []byte, posting *Posting) error {
	if err := json.Unmarshal(line, posting); err != nil {
		return err
	}
	// 1.1.6 wrote a timestamp into every record. A record without a valid
	// one is not a record of that format.
	if _, err := time.Parse(time.RFC3339Nano, posting.Timestamp); err != nil {
		return errInvalidRecord
	}
	return nil
}

func (jsonCodec) keysFilter(keys ...string) LineFilter { return matchKeysFilter(keys...) }

func (jsonCodec) pairFilter(subject, eventType string) LineFilter {
	return matchSubjectAndTypeFilter(subject, eventType)
}

// compactCodec writes one record per line as eight tab separated fields:
//
//	key, subject, type, event ID, log file, offset, length, event hash prefix
//
// Subject, type and event ID are written empty when they equal the key,
// because none of them can be empty in a stored event. The hash is shortened
// to its first hashPrefixLength hex digits: it only verifies that a location
// points at the record of the posting, the hash chain itself lives in the
// event log. Tab, newline,
// carriage return and backslash are escaped with a backslash. Offset and
// length are decimal. Event time and commit group are not stored, reads use
// neither. A record is about half the size of a JSON record and decodes
// without reflection.
type compactCodec struct{}

const compactFieldCount = 8

// hashPrefixLength is the number of hex digits of the event hash stored in a
// compact record, 64 bits.
const hashPrefixLength = 16

func (compactCodec) version() int { return compactRecordVersion }

func (compactCodec) encode(posting *Posting) []byte {
	buffer := make([]byte, 0, 64+len(posting.Key)+len(posting.Subject)+len(posting.EventType)+len(posting.EventID)+len(posting.EventHash))
	buffer = appendEscaped(buffer, posting.Key)
	buffer = append(buffer, '\t')
	buffer = appendUnlessKey(buffer, posting.Subject, posting.Key)
	buffer = append(buffer, '\t')
	buffer = appendUnlessKey(buffer, posting.EventType, posting.Key)
	buffer = append(buffer, '\t')
	buffer = appendUnlessKey(buffer, posting.EventID, posting.Key)
	buffer = append(buffer, '\t')
	buffer = appendEscaped(buffer, posting.LogFile)
	buffer = append(buffer, '\t')
	buffer = strconv.AppendInt(buffer, posting.Offset, 10)
	buffer = append(buffer, '\t')
	buffer = strconv.AppendInt(buffer, posting.Length, 10)
	buffer = append(buffer, '\t')
	hash := posting.EventHash
	if len(hash) > hashPrefixLength {
		hash = hash[:hashPrefixLength]
	}
	buffer = appendEscaped(buffer, hash)
	return append(buffer, '\n')
}

func appendUnlessKey(buffer []byte, value, key string) []byte {
	if value == key {
		return buffer
	}
	return appendEscaped(buffer, value)
}

func (compactCodec) decode(line []byte, posting *Posting) error {
	var fields [compactFieldCount][]byte
	rest := line
	for position := 0; position < compactFieldCount-1; position++ {
		separator := bytes.IndexByte(rest, '\t')
		if separator < 0 {
			return errInvalidRecord
		}
		fields[position], rest = rest[:separator], rest[separator+1:]
	}
	if bytes.IndexByte(rest, '\t') >= 0 {
		return errInvalidRecord
	}
	fields[compactFieldCount-1] = rest
	key, err := unescape(fields[0])
	if err != nil {
		return err
	}
	*posting = Posting{Key: key}
	if posting.Subject, err = unescapeOrKey(fields[1], key); err != nil {
		return err
	}
	if posting.EventType, err = unescapeOrKey(fields[2], key); err != nil {
		return err
	}
	if posting.EventID, err = unescapeOrKey(fields[3], key); err != nil {
		return err
	}
	if posting.LogFile, err = unescape(fields[4]); err != nil {
		return err
	}
	if posting.Offset, err = parseDecimal(fields[5]); err != nil {
		return err
	}
	if posting.Length, err = parseDecimal(fields[6]); err != nil {
		return err
	}
	if posting.EventHash, err = unescape(fields[7]); err != nil {
		return err
	}
	if posting.Subject == "" || posting.EventType == "" || posting.EventID == "" {
		return errInvalidRecord
	}
	return nil
}

func (compactCodec) keysFilter(keys ...string) LineFilter {
	prefixes := make([][]byte, 0, len(keys))
	for _, key := range keys {
		prefixes = append(prefixes, append(appendEscaped(nil, key), '\t'))
	}
	return prefixFilter(prefixes)
}

// pairFilter matches records with an empty key, which only the pair category
// and the catalog write, followed by the exact subject and type.
func (compactCodec) pairFilter(subject, eventType string) LineFilter {
	prefix := []byte{'\t'}
	prefix = appendEscaped(prefix, subject)
	prefix = append(prefix, '\t')
	prefix = appendEscaped(prefix, eventType)
	prefix = append(prefix, '\t')
	return prefixFilter([][]byte{prefix})
}

func prefixFilter(prefixes [][]byte) LineFilter {
	if len(prefixes) == 1 {
		prefix := prefixes[0]
		return func(line []byte) bool { return bytes.HasPrefix(line, prefix) }
	}
	return func(line []byte) bool {
		for _, prefix := range prefixes {
			if bytes.HasPrefix(line, prefix) {
				return true
			}
		}
		return false
	}
}

// appendEscaped appends value with tab, newline, carriage return and
// backslash escaped. The escaping is injective, so a key prefix of one value
// never matches the record of another.
func appendEscaped(buffer []byte, value string) []byte {
	if !strings.ContainsAny(value, "\t\n\r\\") {
		return append(buffer, value...)
	}
	for position := 0; position < len(value); position++ {
		switch character := value[position]; character {
		case '\t':
			buffer = append(buffer, '\\', 't')
		case '\n':
			buffer = append(buffer, '\\', 'n')
		case '\r':
			buffer = append(buffer, '\\', 'r')
		case '\\':
			buffer = append(buffer, '\\', '\\')
		default:
			buffer = append(buffer, character)
		}
	}
	return buffer
}

func unescape(field []byte) (string, error) {
	if bytes.IndexByte(field, '\\') < 0 {
		return string(field), nil
	}
	value := make([]byte, 0, len(field))
	for position := 0; position < len(field); position++ {
		if field[position] != '\\' {
			value = append(value, field[position])
			continue
		}
		position++
		if position == len(field) {
			return "", errInvalidRecord
		}
		switch field[position] {
		case 't':
			value = append(value, '\t')
		case 'n':
			value = append(value, '\n')
		case 'r':
			value = append(value, '\r')
		case '\\':
			value = append(value, '\\')
		default:
			return "", errInvalidRecord
		}
	}
	return string(value), nil
}

func unescapeOrKey(field []byte, key string) (string, error) {
	if len(field) == 0 {
		return key, nil
	}
	return unescape(field)
}

func parseDecimal(field []byte) (int64, error) {
	if len(field) == 0 || len(field) > 19 {
		return 0, errInvalidRecord
	}
	var value int64
	for _, digit := range field {
		if digit < '0' || digit > '9' {
			return 0, errInvalidRecord
		}
		value = value*10 + int64(digit-'0')
	}
	return value, nil
}
