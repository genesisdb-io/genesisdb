package index

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/fsutil"
	"github.com/genesisdb-io/genesisdb/v2/packages/core/layout"
)

// The index root may contain three kinds of data besides the active index:
//
//   - the pre 1.1.6 layout: one file per subject or type, named by the
//     SHA-256 of the key, plus nested per type directories
//   - the retired latest-event category of earlier sharded builds
//   - index files of the other storage format, left behind by a conversion
//     between the legacy and the current format.
//
// All of them are derived data and are removed before a rebuild. Anything
// that is not recognized stops the startup instead of being deleted.

// layoutInventory is the result of classifying the index root.
type layoutInventory struct {
	disposableFiles       int64
	disposableDirectories int64
	activeShardFiles      int
	invalidActiveFiles    int
	hasTemporaryState     bool
}

func (inventory *layoutInventory) hasDisposableData() bool {
	return inventory.disposableFiles > 0 || inventory.disposableDirectories > 0
}

// entryKind classifies one file of a category directory.
type entryKind int

const (
	entryActive entryKind = iota
	entryInvalidActive
	entryDisposable
)

func (index *Index) otherFormat() layout.Format {
	if index.format == layout.FormatLegacy {
		return layout.FormatCurrent
	}
	return layout.FormatLegacy
}

func unrecognizedEntryError(path, problem string) error {
	return fmt.Errorf("index directory contains %s at %s; move it out of the index root (or remove it if it is not needed) and restart. GenesisDB will not delete unrecognized data", problem, path)
}

func futureFormatError(path string, header *ShardHeader) error {
	return fmt.Errorf("%w: %s has format version %d with %d shards; upgrade GenesisDB instead of downgrading", core.ErrUnsupportedIndexFormat, path, header.Version, header.ShardCount)
}

// classifyCategoryFile classifies a regular file inside a category
// directory. It never modifies anything.
func (index *Index) classifyCategoryFile(category, name, path string) (entryKind, error) {
	other := index.otherFormat()
	readHeader := func() (shardHeaderKind, *ShardHeader, error) {
		line, err := fsutil.ReadFirstLine(path)
		if err != nil {
			return 0, nil, err
		}
		kind, header := classifyShardHeader(line)
		if kind == shardHeaderFuture {
			return kind, header, futureFormatError(path, header)
		}
		return kind, header, nil
	}

	if category == categorySubjectCatalog {
		switch name {
		case index.format.IndexCatalogFile():
			kind, _, err := readHeader()
			if err != nil {
				return 0, err
			}
			switch kind {
			case shardHeaderCurrent:
				return entryActive, nil
			case shardHeaderLegacy:
				return entryDisposable, nil
			case shardHeaderEmpty:
				// An interrupted legacy write or an unpublished catalog.
				return entryInvalidActive, nil
			}
			return 0, unrecognizedEntryError(path, "a file with an unrecognized header")
		case other.IndexCatalogFile():
			kind, _, err := readHeader()
			if err != nil {
				return 0, err
			}
			if kind == shardHeaderCurrent || kind == shardHeaderLegacy || kind == shardHeaderEmpty {
				return entryDisposable, nil
			}
			return 0, unrecognizedEntryError(path, "a file with an unrecognized header")
		}
		return 0, unrecognizedEntryError(path, "an unknown file")
	}

	if layout.LegacyHashedIndexFile(name) && (category == categoryEventsBySubject || category == categoryEventsBySubjectWithAncestors || category == categoryEventTypes) {
		return entryDisposable, nil
	}
	if _, isShard := index.format.ParseShardFileName(name); isShard {
		kind, _, err := readHeader()
		if err != nil {
			return 0, err
		}
		if kind != shardHeaderCurrent {
			return entryInvalidActive, nil
		}
		return entryActive, nil
	}
	if _, isShard := other.ParseShardFileName(name); isShard {
		kind, _, err := readHeader()
		if err != nil {
			return 0, err
		}
		if kind == shardHeaderCurrent || kind == shardHeaderEmpty {
			return entryDisposable, nil
		}
		return 0, unrecognizedEntryError(path, "a file with an unrecognized header")
	}
	return 0, unrecognizedEntryError(path, "an unknown file")
}

// isRetiredLatestEventFile reports whether a file of the retired
// latest-event category is recognized disposable data.
func isRetiredLatestEventFile(path, name string) (bool, error) {
	_, isLegacyShard := layout.FormatLegacy.ParseShardFileName(name)
	_, isCurrentShard := layout.FormatCurrent.ParseShardFileName(name)
	isShard := isLegacyShard || isCurrentShard
	if name != layout.LegacyIndexFile && !isShard {
		return false, nil
	}
	line, err := fsutil.ReadFirstLine(path)
	if err != nil {
		return false, err
	}
	kind, header := classifyShardHeader(line)
	switch kind {
	case shardHeaderFuture:
		return false, futureFormatError(path, header)
	case shardHeaderEmpty:
		return true, nil
	case shardHeaderLegacy:
		return name == layout.LegacyIndexFile, nil
	case shardHeaderCurrent:
		return isShard && header.Category == categoryRetiredLatestEvent, nil
	}
	return false, nil
}

// isOtherFormatStateFile reports whether a root file is the state journal
// of the other storage format.
func (index *Index) isOtherFormatStateFile(name string) bool {
	other := index.otherFormat()
	return name == other.IndexStateFile() || name == other.TemporaryIndexStateFile()
}

// inspectLayout classifies every entry of the index root without modifying
// it. Unknown entries, symlinks and future formats are errors.
func (index *Index) inspectLayout() (*layoutInventory, error) {
	inventory := &layoutInventory{}
	info, err := os.Lstat(index.root)
	if errors.Is(err, os.ErrNotExist) {
		return inventory, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("index root %s is a symlink; symlinked index roots are not supported", index.root)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("index root %s is not a directory", index.root)
	}
	err = fsutil.VisitDirectory(index.root, func(name string, mode os.FileMode) error {
		path := filepath.Join(index.root, name)
		if mode&os.ModeSymlink != 0 {
			return unrecognizedEntryError(path, "a symlink")
		}
		switch {
		case name == index.format.IndexStateFile():
			if !mode.IsRegular() {
				return unrecognizedEntryError(path, "a non-regular state journal")
			}
			return nil
		case name == index.format.TemporaryIndexStateFile():
			if !mode.IsRegular() {
				return unrecognizedEntryError(path, "a non-regular file")
			}
			inventory.hasTemporaryState = true
			return nil
		case index.isOtherFormatStateFile(name):
			if !mode.IsRegular() {
				return unrecognizedEntryError(path, "a non-regular file")
			}
			inventory.disposableFiles++
			return nil
		}
		if !mode.IsDir() {
			return unrecognizedEntryError(path, "an unknown file")
		}
		if name == categoryRetiredLatestEvent {
			inventory.disposableDirectories++
			return index.inspectRetiredLatestEventDirectory(path, inventory)
		}
		if !isCategoryDirectory(name) {
			return unrecognizedEntryError(path, "an unknown directory")
		}
		return index.inspectCategoryDirectory(name, inventory)
	})
	if err != nil {
		return nil, err
	}
	return inventory, nil
}

func isCategoryDirectory(name string) bool {
	for _, category := range categoryDirectories {
		if category == name {
			return true
		}
	}
	return false
}

func (index *Index) inspectRetiredLatestEventDirectory(directory string, inventory *layoutInventory) error {
	return fsutil.VisitDirectory(directory, func(name string, mode os.FileMode) error {
		path := filepath.Join(directory, name)
		if mode&os.ModeSymlink != 0 {
			return unrecognizedEntryError(path, "a symlink")
		}
		if !mode.IsRegular() {
			return unrecognizedEntryError(path, "an unknown entry")
		}
		isDisposable, err := isRetiredLatestEventFile(path, name)
		if err != nil {
			return err
		}
		if !isDisposable {
			return unrecognizedEntryError(path, "a file with an unrecognized header")
		}
		inventory.disposableFiles++
		return nil
	})
}

func (index *Index) inspectCategoryDirectory(category string, inventory *layoutInventory) error {
	directory := filepath.Join(index.root, category)
	return fsutil.VisitDirectory(directory, func(name string, mode os.FileMode) error {
		path := filepath.Join(directory, name)
		if mode&os.ModeSymlink != 0 {
			return unrecognizedEntryError(path, "a symlink")
		}
		if mode.IsDir() {
			if category == categoryLatestEventOfSubjectAndType && layout.IsHex64(name) {
				inventory.disposableDirectories++
				return inspectLegacyTypeDirectory(path, inventory)
			}
			return unrecognizedEntryError(path, "an unknown directory")
		}
		if !mode.IsRegular() {
			return unrecognizedEntryError(path, "a non-regular file")
		}
		kind, err := index.classifyCategoryFile(category, name, path)
		if err != nil {
			return err
		}
		switch kind {
		case entryActive:
			if category != categorySubjectCatalog {
				inventory.activeShardFiles++
			}
		case entryInvalidActive:
			inventory.invalidActiveFiles++
			if category != categorySubjectCatalog {
				inventory.activeShardFiles++
			}
		case entryDisposable:
			inventory.disposableFiles++
		}
		return nil
	})
}

// inspectLegacyTypeDirectory validates the pre 1.1.6 tree
// latest-event-of-type/<type hash>/<subject hash>/index.eventstore.
func inspectLegacyTypeDirectory(typeDirectory string, inventory *layoutInventory) error {
	return fsutil.VisitDirectory(typeDirectory, func(name string, mode os.FileMode) error {
		path := filepath.Join(typeDirectory, name)
		if mode&os.ModeSymlink != 0 {
			return unrecognizedEntryError(path, "a symlink")
		}
		if !mode.IsDir() || !layout.IsHex64(name) {
			return unrecognizedEntryError(path, "an unknown legacy entry")
		}
		inventory.disposableDirectories++
		return fsutil.VisitDirectory(path, func(fileName string, fileMode os.FileMode) error {
			filePath := filepath.Join(path, fileName)
			if fileName != layout.LegacyIndexFile || !fileMode.IsRegular() {
				return unrecognizedEntryError(filePath, "an unknown legacy entry")
			}
			inventory.disposableFiles++
			return nil
		})
	})
}

// removeDisposableData deletes every recognized disposable file and
// directory. It never removes the index root or a category directory, never
// follows symlinks and is safe to retry.
func (index *Index) removeDisposableData() error {
	err := fsutil.VisitDirectory(index.root, func(name string, mode os.FileMode) error {
		if index.isOtherFormatStateFile(name) && mode.IsRegular() {
			return removeFile(filepath.Join(index.root, name))
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, category := range categoriesWithDisposableData {
		directory := filepath.Join(index.root, category)
		if _, err := os.Lstat(directory); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if category == categoryRetiredLatestEvent {
			if err := index.removeRetiredLatestEventDirectory(directory); err != nil {
				return err
			}
			continue
		}
		if err := index.removeDisposableDataIn(category, directory); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(index.root); err == nil {
		return fsutil.SyncDirectory(index.root)
	}
	return nil
}

func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove disposable index file %s: %w", path, err)
	}
	return nil
}

func (index *Index) removeRetiredLatestEventDirectory(directory string) error {
	// Repeat passes because unlinking while reading a directory may skip
	// entries.
	for {
		removed := 0
		err := fsutil.VisitDirectory(directory, func(name string, mode os.FileMode) error {
			path := filepath.Join(directory, name)
			if mode&os.ModeSymlink != 0 || !mode.IsRegular() {
				return unrecognizedEntryError(path, "an unknown entry")
			}
			isDisposable, err := isRetiredLatestEventFile(path, name)
			if err != nil {
				return err
			}
			if !isDisposable {
				return unrecognizedEntryError(path, "a file with an unrecognized header")
			}
			removed++
			return removeFile(path)
		})
		if err != nil {
			return err
		}
		if removed == 0 {
			break
		}
	}
	if err := os.Remove(directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove legacy index directory %s: %w", directory, err)
	}
	return fsutil.SyncDirectory(index.root)
}

func (index *Index) removeDisposableDataIn(category, directory string) error {
	for {
		removed := 0
		err := fsutil.VisitDirectory(directory, func(name string, mode os.FileMode) error {
			path := filepath.Join(directory, name)
			if mode&os.ModeSymlink != 0 {
				return unrecognizedEntryError(path, "a symlink")
			}
			if mode.IsDir() {
				if category == categoryLatestEventOfSubjectAndType && layout.IsHex64(name) {
					removed++
					return removeLegacyTypeDirectory(path)
				}
				return nil
			}
			if !mode.IsRegular() {
				return nil
			}
			kind, err := index.classifyCategoryFile(category, name, path)
			if err != nil || kind != entryDisposable {
				return nil
			}
			removed++
			return removeFile(path)
		})
		if err != nil {
			return err
		}
		if removed == 0 {
			break
		}
	}
	return fsutil.SyncDirectory(directory)
}

func removeLegacyTypeDirectory(typeDirectory string) error {
	for {
		removed := 0
		err := fsutil.VisitDirectory(typeDirectory, func(name string, mode os.FileMode) error {
			path := filepath.Join(typeDirectory, name)
			if !mode.IsDir() || !layout.IsHex64(name) {
				return unrecognizedEntryError(path, "an unknown legacy entry")
			}
			if err := fsutil.VisitDirectory(path, func(fileName string, fileMode os.FileMode) error {
				filePath := filepath.Join(path, fileName)
				if fileName != layout.LegacyIndexFile || !fileMode.IsRegular() {
					return unrecognizedEntryError(filePath, "an unknown legacy entry")
				}
				return removeFile(filePath)
			}); err != nil {
				return err
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove legacy index directory %s: %w", path, err)
			}
			removed++
			return nil
		})
		if err != nil {
			return err
		}
		if removed == 0 {
			break
		}
	}
	if err := os.Remove(typeDirectory); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove legacy index directory %s: %w", typeDirectory, err)
	}
	return nil
}

// ensureCategoryDirectories creates missing category directories.
func (index *Index) ensureCategoryDirectories() error {
	if err := os.MkdirAll(index.root, 0o700); err != nil {
		return fmt.Errorf("create index root: %w", err)
	}
	isCreated := false
	for _, category := range categoryDirectories {
		path := filepath.Join(index.root, category)
		info, err := os.Lstat(path)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return unrecognizedEntryError(path, "a non-directory category entry")
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := index.faults.CheckCreate(path); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("create index category %s: %w", category, err)
		}
		isCreated = true
	}
	if isCreated {
		return fsutil.SyncDirectory(index.root)
	}
	return nil
}

// truncateUnpublishedWrites truncates every index file beyond its published
// boundary in place. Inodes are kept, so rebuilds never need spare inodes.
func (index *Index) truncateUnpublishedWrites(published map[string]int64) error {
	for _, relativePath := range index.paths.all() {
		path := index.path(relativePath)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return unrecognizedEntryError(path, "a non-regular file")
		}
		boundary := published[relativePath]
		if info.Size() <= boundary {
			continue
		}
		if err := fsutil.TruncateAndSync(path, boundary); err != nil {
			return err
		}
	}
	return nil
}
