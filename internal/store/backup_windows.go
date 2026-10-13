package store

// Windows syncs the backup file; directory fsync is a Unix operation.
func syncBackupDirectory(string) error { return nil }
