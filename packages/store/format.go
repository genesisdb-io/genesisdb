package store

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
	"github.com/genesisdb-io/genesisdb/v2/packages/storage"
)

// prepareStorageFormat detects the storage format of the data directory,
// finishes an interrupted conversion and applies the policy.
//
// Detection order:
//  1. format.gdb with a running conversion: finish it.
//  2. format.gdb: current format.
//  3. events/events.eventstore: legacy format.
//  4. no event log: a new directory in the current format.
//
// Logs of both formats without a running conversion are refused.
func (s *Store) prepareStorageFormat(policy FormatPolicy) (layout.Format, error) {
	format, err := detectStorageFormat(s.dataDirectory, s.logger)
	if err != nil {
		return 0, err
	}
	if format == layout.FormatLegacy {
		if policy == ConvertToCurrentFormat {
			s.logger.Info("migrating data directory from GenesisDB 1.x storage format")
			if _, err := s.convertWhileLocked(layout.FormatCurrent, false); err != nil {
				return 0, err
			}
			return layout.FormatCurrent, nil
		}
		s.logger.Warn("data directory is kept in the legacy storage format (GENESISDB_STORAGE_FORMAT=keep); new events do not receive predecessor-bound hashes")
		return layout.FormatLegacy, nil
	}
	if marker, err := layout.ReadFormatMarker(s.dataDirectory); errors.Is(err, layout.ErrNoFormatMarker) || (err == nil && marker.Version < layout.CurrentFormatVersion) {
		if err := writeFormatMarker(s.dataDirectory, layout.NewFormatMarker()); err != nil {
			return 0, err
		}
	}
	return layout.FormatCurrent, nil
}

// detectStorageFormat returns the format of a data directory and finishes an
// interrupted conversion first.
func detectStorageFormat(dataDirectory string, logger interface{ Info(string, ...any) }) (layout.Format, error) {
	marker, err := layout.ReadFormatMarker(dataDirectory)
	if err != nil && !errors.Is(err, layout.ErrNoFormatMarker) {
		return 0, err
	}
	if err == nil && marker.Version > layout.CurrentFormatVersion {
		return 0, fmt.Errorf("%w: format version %d is newer than supported", core.ErrUnsupportedStorageFormat, marker.Version)
	}
	if err == nil && marker.IsConverting() {
		target := marker.ConversionTarget()
		logger.Info("finishing an interrupted storage format conversion", "target", target.String())
		if _, err := convertLogs(dataDirectory, otherFormat(target), target, false); err != nil {
			return 0, fmt.Errorf("finish interrupted storage format conversion: %w", err)
		}
		return target, nil
	}
	hasLegacyLogs, err := hasLogsOfFormat(dataDirectory, layout.FormatLegacy)
	if err != nil {
		return 0, err
	}
	hasCurrentLogs, err := hasLogsOfFormat(dataDirectory, layout.FormatCurrent)
	if err != nil {
		return 0, err
	}
	if hasLegacyLogs && hasCurrentLogs {
		return 0, fmt.Errorf("%w: %s contains event logs of both formats (.eventstore and .gdb) without a running conversion; keep one set and move the other out of the directory", core.ErrUnsupportedStorageFormat, filepath.Join(dataDirectory, layout.EventsDirectory))
	}
	if err == nil && marker.Format != "" {
		if marker.Version > layout.CurrentFormatVersion {
			return 0, fmt.Errorf("%w: format version %d is newer than this GenesisDB supports (%d); upgrade GenesisDB", core.ErrUnsupportedStorageFormat, marker.Version, layout.CurrentFormatVersion)
		}
		if hasLegacyLogs {
			return 0, fmt.Errorf("%w: %s marks the current format but the event logs use the legacy format", core.ErrUnsupportedStorageFormat, layout.FormatMarkerFile)
		}
		return layout.FormatCurrent, nil
	}
	if hasLegacyLogs {
		return layout.FormatLegacy, nil
	}
	return layout.FormatCurrent, nil
}

func otherFormat(format layout.Format) layout.Format {
	if format == layout.FormatLegacy {
		return layout.FormatCurrent
	}
	return layout.FormatLegacy
}

func hasLogsOfFormat(dataDirectory string, format layout.Format) (bool, error) {
	isFound := false
	err := fsutil.VisitDirectory(filepath.Join(dataDirectory, layout.EventsDirectory), func(name string, mode os.FileMode) error {
		if format.IsLogFile(name) && mode.IsRegular() {
			isFound = true
			return fsutil.ErrStopScan
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, fsutil.ErrStopScan) {
		return isFound, nil
	}
	return isFound, err
}

func writeFormatMarker(dataDirectory string, marker layout.FormatMarker) error {
	return fsutil.ReplaceAtomically(filepath.Join(dataDirectory, layout.FormatMarkerFile), marker.Encode(), nil, nil, nil)
}

// ConversionReport describes a storage format conversion.
type ConversionReport struct {
	From           layout.Format
	To             layout.Format
	ConvertedFiles []string
	IsDryRun       bool
}

// Convert migrates a closed legacy data directory to the current storage
// format without starting a server. Open does the same automatically, this
// is the explicit offline variant. It is crash safe and resumes when it is
// run again. Indexes are rebuilt afterwards, because the log header length
// and with it every record offset changes.
func Convert(dataDirectory string, target layout.Format, isDryRun bool, options Options) (*ConversionReport, error) {
	lock, err := fsutil.LockDirectory(dataDirectory)
	if err != nil {
		return nil, err
	}
	s := &Store{dataDirectory: dataDirectory, lock: lock, logger: options.Logger, faults: options.Faults}
	if s.logger == nil {
		s.logger = discardLogger()
	}
	report, err := s.convertWhileLocked(target, isDryRun)
	lock.Unlock()
	if err != nil || isDryRun || report.From == report.To {
		return report, err
	}
	options.FormatPolicy = KeepExistingFormat
	converted, err := Open(dataDirectory, options)
	if err != nil {
		return report, fmt.Errorf("logs converted, but rebuilding indexes failed: %w", err)
	}
	return report, converted.Close()
}

// convertWhileLocked converts the logs of a locked data directory.
func (s *Store) convertWhileLocked(target layout.Format, isDryRun bool) (*ConversionReport, error) {
	source, err := detectStorageFormat(s.dataDirectory, s.logger)
	if err != nil {
		return nil, err
	}
	report := &ConversionReport{From: source, To: target, IsDryRun: isDryRun}
	if source == target {
		return report, nil
	}
	if !isDryRun {
		// Replay the write-ahead log and repair interrupted records in the
		// source format first, so only complete logs are converted.
		if _, err := storage.Open(s.dataDirectory, storage.Options{Format: source, Logger: s.logger, Faults: s.faults}); err != nil {
			return nil, err
		}
	}
	converted, err := convertLogs(s.dataDirectory, source, target, isDryRun)
	report.ConvertedFiles = converted
	if err == nil && !isDryRun {
		s.logger.Info("storage format converted", "from", source.String(), "to", target.String(), "files", len(converted))
	}
	return report, err
}

// convertLogs rewrites every legacy log with the header and extension of the
// current format. Only the upgrade direction exists, GenesisDB 2 data is not
// meant to be opened by 1.x again.
//
//  1. format.gdb is written with conversion "in-progress" and the target.
//  2. Each log is written to <name>.gdb.tmp with the new header and the
//     unchanged body, synced, renamed, and verified byte for byte against
//     the source body before the source is removed.
//  3. format.gdb is finalized.
//
// Every step is idempotent, so an interrupted conversion resumes.
func convertLogs(dataDirectory string, source, target layout.Format, isDryRun bool) ([]string, error) {
	eventsDirectory := filepath.Join(dataDirectory, layout.EventsDirectory)
	var sourceLogs []string
	err := fsutil.VisitDirectory(eventsDirectory, func(name string, mode os.FileMode) error {
		if source.IsLogFile(name) && mode.IsRegular() {
			sourceLogs = append(sourceLogs, name)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	sort.Strings(sourceLogs)
	if target != layout.FormatCurrent {
		return nil, fmt.Errorf("%w: conversion to the legacy format is not supported", core.ErrUnsupportedStorageFormat)
	}
	if isDryRun {
		return sourceLogs, nil
	}
	marker := layout.NewFormatMarker()
	marker.Conversion = layout.ConversionInProgress
	marker.Target = target.String()
	if err := writeFormatMarker(dataDirectory, marker); err != nil {
		return nil, err
	}
	for _, sourceName := range sourceLogs {
		targetName := source.LogBaseName(sourceName) + target.Extension()
		if err := convertLog(eventsDirectory, sourceName, targetName, target); err != nil {
			return nil, fmt.Errorf("convert %s: %w", sourceName, err)
		}
	}
	if err := fsutil.SyncDirectory(eventsDirectory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return sourceLogs, writeFormatMarker(dataDirectory, layout.NewFormatMarker())
}

func convertLog(eventsDirectory, sourceName, targetName string, target layout.Format) error {
	sourcePath := filepath.Join(eventsDirectory, sourceName)
	targetPath := filepath.Join(eventsDirectory, targetName)
	temporaryPath := targetPath + layout.TemporaryFileSuffix
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sourceChecksum, err := logBodyChecksum(sourcePath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(targetPath); err == nil {
		// A previous run renamed the target but stopped before removing
		// the source.
		targetChecksum, err := logBodyChecksum(targetPath)
		if err != nil {
			return err
		}
		if targetChecksum != sourceChecksum {
			return fmt.Errorf("%s and %s differ; inspect both before converting again", sourcePath, targetPath)
		}
		return fsutil.RemoveAndSync(sourcePath)
	}
	if err := copyLogWithHeader(sourcePath, temporaryPath, target); err != nil {
		os.Remove(temporaryPath)
		return err
	}
	temporaryChecksum, err := logBodyChecksum(temporaryPath)
	if err != nil {
		return err
	}
	if temporaryChecksum != sourceChecksum {
		os.Remove(temporaryPath)
		return fmt.Errorf("verification of %s failed", temporaryPath)
	}
	if err := os.Rename(temporaryPath, targetPath); err != nil {
		return err
	}
	if err := fsutil.SyncDirectory(eventsDirectory); err != nil {
		return err
	}
	return fsutil.RemoveAndSync(sourcePath)
}

// copyLogWithHeader writes the target header and the body of source to
// destination and syncs it.
func copyLogWithHeader(sourcePath, destinationPath string, target layout.Format) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	reader := bufio.NewReader(source)
	header, err := reader.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return err
	}
	if len(header) > 0 && !layout.IsLogHeader(bytes.TrimSuffix(header, []byte("\n"))) {
		return fmt.Errorf("%s has an invalid header", sourcePath)
	}
	destination, err := os.OpenFile(destinationPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := destination.Write(target.LogHeaderLine()); err != nil {
		destination.Close()
		return err
	}
	if _, err := io.Copy(destination, reader); err != nil {
		destination.Close()
		return err
	}
	if err := destination.Sync(); err != nil {
		destination.Close()
		return err
	}
	return destination.Close()
}

// logBodyChecksum returns the SHA-256 of a log without its header line.
func logBodyChecksum(path string) ([sha256.Size]byte, error) {
	var checksum [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return checksum, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	if _, err := reader.ReadBytes('\n'); err != nil && err != io.EOF {
		return checksum, err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return checksum, err
	}
	copy(checksum[:], hash.Sum(nil))
	return checksum, nil
}
