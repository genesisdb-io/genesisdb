package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/shirou/gopsutil/v4/cpu"

	"github.com/genesisdb-io/genesisdb/v2/packages/core"
	"github.com/genesisdb-io/genesisdb/v2/packages/store"
)

func (server *Server) handleVerifyHashChain(writer http.ResponseWriter, request *http.Request) {
	report, err := server.store.VerifyHashChain(request.Context())
	if err != nil {
		http.Error(writer, fmt.Sprintf("Error auditing events: %v", err), httpStatusForError(err))
		return
	}
	eventCount := 0
	if count, err := server.store.CountEvents(); err == nil {
		eventCount = int(count)
	}
	writer.Header().Set("Content-Type", "text/plain")
	if report.IsValid() {
		fmt.Fprintf(writer, "Audit passed. All %d events integrity checks succeeded.", eventCount)
		return
	}
	// The violation is reported once per subject, as in earlier releases.
	fmt.Fprintf(writer, "Audit failed. Found %d integrity issues across %d events:\n", len(report.Subjects), eventCount)
	for _, subject := range report.Subjects {
		fmt.Fprintf(writer, "- Subject %s: %s\n", subject, report.Problem)
	}
}

func (server *Server) handleRebuildIndexes(writer http.ResponseWriter, request *http.Request) {
	count, err := server.store.RebuildIndexes(request.Context())
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to rebuild indexes (store unavailable until recovery): %v", err), httpStatusForError(err))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{
		"status":  "success",
		"message": fmt.Sprintf("Successfully rebuilt indexes for %d events", count),
	})
}

// EraseRequest is the body of POST /api/v2/admin/erase and /api/v1/erase.
type EraseRequest struct {
	Subject string `json:"subject"`
}

func (server *Server) handleErase(writer http.ResponseWriter, request *http.Request) {
	var body EraseRequest
	if !decodeRequestBody(writer, request, &body) {
		return
	}
	if strings.TrimSpace(body.Subject) == "" {
		http.Error(writer, "Subject cannot be empty", http.StatusBadRequest)
		return
	}
	if !strings.HasPrefix(strings.TrimSpace(body.Subject), "/") {
		http.Error(writer, "Subject must start with /", http.StatusBadRequest)
		return
	}
	report, err := server.store.Erase(request.Context(), body.Subject)
	if errors.Is(err, core.ErrNotFound) {
		http.Error(writer, "No events found for subject", http.StatusNotFound)
		return
	}
	if errors.Is(err, store.ErrNothingToErase) {
		http.Error(writer, "Subject has no erasable reference data, inline event data is immutable", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		http.Error(writer, fmt.Sprintf("Failed to erase ref data: %v", err), httpStatusForError(err))
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(writer, "Erased %d ref data files for subject %s", report.ErasedEntries, body.Subject)
}

// StatusResponse is the body of GET /api/v2/status and /api/v1/status. The field order is part
// of the API.
type StatusResponse struct {
	Engine  EngineStatus  `json:"engine"`
	System  SystemStatus  `json:"system"`
	Events  EventsStatus  `json:"events"`
	Storage StorageStatus `json:"storage"`
}

// EngineStatus describes the running GenesisDB.
type EngineStatus struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
}

// SystemStatus describes the host.
type SystemStatus struct {
	OS      string        `json:"os"`
	Arch    string        `json:"arch"`
	CPU     CPUStatus     `json:"cpu"`
	Memory  MemoryStatus  `json:"memory"`
	Storage CapacityUsage `json:"storage"`
}

// CPUStatus describes processor usage.
type CPUStatus struct {
	AvailableCores int64 `json:"availableCores"`
	UsedCores      int64 `json:"usedCores"`
}

// MemoryStatus describes the memory of the Go runtime.
type MemoryStatus struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"`
	Available uint64 `json:"available"`
}

// CapacityUsage describes the volume of the data directory in bytes.
type CapacityUsage struct {
	Max       int64 `json:"max"`
	Used      int64 `json:"used"`
	Available int64 `json:"available"`
}

// EventsStatus summarizes the stored events.
type EventsStatus struct {
	Count       int   `json:"count"`
	Subjects    int   `json:"subjects"`
	Types       int   `json:"types"`
	StorageSize int64 `json:"storageSize"`
}

// StorageStatus describes the data directory.
type StorageStatus struct {
	// Format is "current" (.gdb files) or "legacy" (.eventstore files).
	Format  string `json:"storageFormat"`
	IsReady bool   `json:"ready"`
}

// status collects the engine, system, event and storage status.
func (server *Server) status(ctx context.Context) StatusResponse {
	response := StatusResponse{
		Engine:  EngineStatus{Version: core.Version, Channel: core.ReleaseChannel},
		Storage: StorageStatus{Format: server.store.StorageFormat().String(), IsReady: server.store.IsReady()},
	}
	if count, err := server.store.CountEvents(); err == nil {
		response.Events.Count = int(count)
	}
	if subjects, err := server.store.ListTopLevelSubjects(ctx); err == nil {
		response.Events.Subjects = len(subjects)
	}
	if eventTypes, err := server.store.ListEventTypes(ctx); err == nil {
		response.Events.Types = len(eventTypes)
	}
	response.Events.StorageSize = server.store.EventLogSizeBytes()

	response.System.OS = runtime.GOOS
	response.System.Arch = runtime.GOARCH
	if capacity, isKnown := volumeCapacity(server.store.DataDirectory()); isKnown {
		response.System.Storage = capacity
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	response.System.Memory = MemoryStatus{Total: memory.Sys, Used: memory.Alloc, Available: memory.Sys - memory.Alloc}
	availableCores := runtime.NumCPU()
	response.System.CPU.AvailableCores = int64(availableCores)
	if percentages, err := cpu.Percent(0, true); err == nil && len(percentages) > 0 {
		total := 0.0
		for _, percentage := range percentages {
			total += percentage
		}
		response.System.CPU.UsedCores = int64(total / float64(len(percentages)) / 100 * float64(availableCores))
	}
	return response
}

func (server *Server) handleDetailedStatus(writer http.ResponseWriter, request *http.Request) {
	writeJSON(writer, http.StatusOK, server.status(request.Context()))
}
