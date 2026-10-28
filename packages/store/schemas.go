package store

import "context"

// UpdateSchemas serializes schema changes with commits, backups and restores.
// The callback must only update schema files and must not call Store methods.
// Embedded applications must use this method for schema changes while open.
func (s *Store) UpdateSchemas(ctx context.Context, update func(directory string) error) error {
	if err := acquireContext(ctx, s.writeMutex.TryLock); err != nil {
		return err
	}
	defer s.writeMutex.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.checkWritable(); err != nil {
		return err
	}
	return update(s.SchemasDirectory())
}
