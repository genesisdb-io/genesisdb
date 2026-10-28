// Package layout is the single place that knows the file names of a GenesisDB
// data directory. No other package hard-codes a file extension or a log
// header.
//
// Two storage formats exist:
//
//   - FormatLegacy: written by GenesisDB 1.1.6 and earlier. Files end in
//     ".eventstore" and logs start with the header line "eventstore".
//   - FormatCurrent: files end in ".gdb", logs start with "genesisdb", and
//     the data directory contains the marker file "format.gdb".
//
// Record contents are identical in both formats.
package layout

import (
	"fmt"
	"strings"
)

// Format is a storage format of a data directory.
type Format int

const (
	// FormatCurrent uses ".gdb" files and the "genesisdb" log header.
	FormatCurrent Format = iota
	// FormatLegacy uses ".eventstore" files and the "eventstore" log header.
	FormatLegacy
)

const (
	currentExtension = ".gdb"
	legacyExtension  = ".eventstore"

	currentLogHeader = "genesisdb"
	legacyLogHeader  = "eventstore"
)

// Directory names inside a data directory. They are format independent.
const (
	EventsDirectory  = "events"
	IndexesDirectory = "indexes"
	SchemasDirectory = "schemas"
	WALDirectory     = "wal"
)

// File names that are format independent.
const (
	// FormatMarkerFile marks a data directory in the current format.
	FormatMarkerFile = "format.gdb"
	// WALFile holds the prepared batch of an in-flight commit.
	WALFile = "pending.wal"
	// TemporaryFileSuffix is appended to a file name while it is rewritten.
	TemporaryFileSuffix = ".tmp"
)

// Base names of the files that exist in every format.
const (
	mainEventLogBaseName     = "events"
	referenceDataLogBaseName = "refs"
	systemEventLogBaseName   = "system"
	indexCatalogBaseName     = "index"
	indexStateBaseName       = "index-state"
)

// String returns "current" or "legacy".
func (f Format) String() string {
	if f == FormatLegacy {
		return "legacy"
	}
	return "current"
}

// ParseFormat parses "current" or "legacy".
func ParseFormat(name string) (Format, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "current", "gdb":
		return FormatCurrent, nil
	case "legacy", "eventstore":
		return FormatLegacy, nil
	}
	return FormatCurrent, fmt.Errorf("unknown storage format %q (expected current or legacy)", name)
}

// Extension returns the file extension of the format, including the dot.
func (f Format) Extension() string {
	if f == FormatLegacy {
		return legacyExtension
	}
	return currentExtension
}

// LogHeader returns the header line (without newline) written to new logs.
func (f Format) LogHeader() string {
	if f == FormatLegacy {
		return legacyLogHeader
	}
	return currentLogHeader
}

// LogHeaderLine returns the header line including its newline.
func (f Format) LogHeaderLine() []byte {
	return []byte(f.LogHeader() + "\n")
}

// IsLogHeader reports whether line is a valid log header of any format.
// Readers accept both headers in every file, independent of its extension.
func IsLogHeader(line []byte) bool {
	text := string(line)
	return text == currentLogHeader || text == legacyLogHeader
}

// IsLogHeaderPrefix reports whether data is the beginning of a log header of
// any format, which is what an interrupted header write leaves behind.
func IsLogHeaderPrefix(data []byte) bool {
	text := string(data)
	return strings.HasPrefix(currentLogHeader+"\n", text) || strings.HasPrefix(legacyLogHeader+"\n", text)
}

// MainEventLogFile returns the file name of the main event log.
func (f Format) MainEventLogFile() string {
	return mainEventLogBaseName + f.Extension()
}

// ReferenceDataLogFile returns the file name of the reference data log.
func (f Format) ReferenceDataLogFile() string {
	return referenceDataLogBaseName + f.Extension()
}

// SystemEventLogFile returns the file name of the system event log.
func (f Format) SystemEventLogFile() string {
	return systemEventLogBaseName + f.Extension()
}

// LogFiles returns the file names of every log that a new store creates.
func (f Format) LogFiles() []string {
	return []string{f.ReferenceDataLogFile(), f.SystemEventLogFile(), f.MainEventLogFile()}
}

// IsLogFile reports whether name has the extension of the format.
func (f Format) IsLogFile(name string) bool {
	return strings.HasSuffix(name, f.Extension()) && len(name) > len(f.Extension())
}

// IsRegularEventLogFile reports whether name is a regular event log of the
// format. Reference data and system event logs are excluded.
func (f Format) IsRegularEventLogFile(name string) bool {
	return f.IsLogFile(name) && name != f.ReferenceDataLogFile() && name != f.SystemEventLogFile()
}

// LogBaseName returns name without the format extension.
func (f Format) LogBaseName(name string) string {
	return strings.TrimSuffix(name, f.Extension())
}

// IndexCatalogFile returns the file name of the subject and type catalog.
func (f Format) IndexCatalogFile() string {
	return indexCatalogBaseName + f.Extension()
}

// IndexStateFile returns the file name of the index state journal.
func (f Format) IndexStateFile() string {
	return indexStateBaseName + f.Extension()
}

// TemporaryIndexStateFile returns the file name used while the index state
// journal is compacted.
func (f Format) TemporaryIndexStateFile() string {
	return f.IndexStateFile() + TemporaryFileSuffix
}

// ShardFileName returns the file name of a shard, "00.gdb" to "ff.gdb".
func (f Format) ShardFileName(shard int) string {
	return fmt.Sprintf("%02x%s", shard, f.Extension())
}

// ParseShardFileName returns the shard number of a valid shard file name of
// the format.
func (f Format) ParseShardFileName(name string) (int, bool) {
	if len(name) != 2+len(f.Extension()) || !strings.HasSuffix(name, f.Extension()) {
		return 0, false
	}
	shard := 0
	for position := 0; position < 2; position++ {
		character := name[position]
		switch {
		case character >= '0' && character <= '9':
			shard = shard*16 + int(character-'0')
		case character >= 'a' && character <= 'f':
			shard = shard*16 + int(character-'a'+10)
		default:
			return 0, false
		}
	}
	return shard, true
}

// LegacyHashedIndexFile reports whether name is a per-key index file of the
// pre 1.1.6 index layout: a 64 character hex SHA-256 plus ".eventstore".
func LegacyHashedIndexFile(name string) bool {
	if !strings.HasSuffix(name, legacyExtension) {
		return false
	}
	return IsHex64(strings.TrimSuffix(name, legacyExtension))
}

// LegacyIndexFile is the file name of the global per-category files of the
// pre 1.1.6 index layout.
const LegacyIndexFile = indexCatalogBaseName + legacyExtension

// IsHex64 reports whether s is a 64 character lowercase hex string.
func IsHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for position := 0; position < len(s); position++ {
		character := s[position]
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f')) {
			return false
		}
	}
	return true
}
