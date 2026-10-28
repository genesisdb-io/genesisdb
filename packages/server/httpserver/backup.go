package httpserver

import (
	"fmt"
	"io"
	"net/http"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
)

// Backup download names. The content is identical to earlier releases,
// restores ignore the name.
const (
	plainBackupFileName    = "genesisdb-backup.json"
	extendedBackupFileName = "genesisdb-backup-extended.json"
)

func (server *Server) handleBackup(writer http.ResponseWriter, request *http.Request) {
	backup, err := server.store.Backup(request.Context())
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to get events: %v", err), httpStatusForError(err))
		return
	}
	fileName := plainBackupFileName
	if backup.Extended {
		fileName = extendedBackupFileName
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Content-Disposition", "attachment; filename="+fileName)
	if err := backup.EncodeJSON(writer); err != nil {
		server.logger.Error("backup response interrupted, client must discard the download", "error", err)
		panic(http.ErrAbortHandler)
	}
}

func (server *Server) handleRestore(writer http.ResponseWriter, request *http.Request) {
	// Emptiness is checked again by the store under maintenance ownership.
	if count, err := server.store.CountEvents(); err != nil {
		http.Error(writer, fmt.Sprintf("Failed to check event store: %v", err), httpStatusForError(err))
		return
	} else if count > 0 {
		http.Error(writer, "Cannot restore backup: Event store is not empty. Restore can only be performed on an empty event store.", http.StatusConflict)
		return
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		requestBodyError(writer, err)
		return
	}
	backup, err := core.DecodeBackup(body)
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to decode events: %v", err), http.StatusBadRequest)
		return
	}
	if err := server.store.Restore(request.Context(), backup); err != nil {
		http.Error(writer, fmt.Sprintf("Failed to import events: %v", err), httpStatusForError(err))
		return
	}
	writer.Header().Set("Content-Type", "text/plain")
	if len(backup.ReferenceData) > 0 || len(backup.SystemEvents) > 0 {
		fmt.Fprintf(writer, "Successfully imported %d events, %d refs, and %d system events", len(backup.Events), len(backup.ReferenceData), len(backup.SystemEvents))
		return
	}
	fmt.Fprintf(writer, "Successfully imported %d events", len(backup.Events))
}
