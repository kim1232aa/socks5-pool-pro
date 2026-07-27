package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// maxSourceImportMultipartOverhead leaves room for browser-generated multipart
// boundaries and headers while the file part itself remains capped at the same
// 16 MiB limit as a remotely fetched source.
const maxSourceImportMultipartOverhead = 64 << 10

type sourceImportResponse struct {
	Source         Source                 `json:"source"`
	CandidateCount int                    `json:"candidate_count"`
	Operation      SourceRefreshOperation `json:"operation"`
	Accepted       bool                   `json:"accepted"`
}

func (s *StatusServer) handleSourceImport(w http.ResponseWriter, r *http.Request) {
	name, data, err := readSourceImportMultipart(w, r)
	if err != nil {
		writeSourceImportRequestError(w, err)
		return
	}

	s.coordinator.sourceLifecycleMu.Lock()
	defer s.coordinator.sourceLifecycleMu.Unlock()
	source, importedTotal, err := s.store.ImportSource(name, bytes.NewReader(data))
	if err != nil {
		switch {
		case errors.Is(err, ErrSourceImportTooLarge):
			writeErrCode(w, http.StatusRequestEntityTooLarge, "source_import_too_large", err)
		case errors.Is(err, ErrSourceImportStorage):
			writeErrCode(w, http.StatusInternalServerError, "source_import_storage_failed", err)
		default:
			writeConfigStoreError(w, err)
		}
		return
	}
	operation, accepted := s.coordinator.requestSourceRefresh(source, "import")
	writeJSONStatus(w, http.StatusAccepted, sourceImportResponse{
		// ImportSource constructs upload sources server-side, so URL is guaranteed
		// empty and there is no remote credential-bearing value to redact.
		Source:         source,
		CandidateCount: importedTotal,
		Operation:      operation,
		Accepted:       accepted,
	})
}

type sourceImportRequestError struct {
	tooLarge bool
	detail   string
}

func (e *sourceImportRequestError) Error() string { return e.detail }

func readSourceImportMultipart(w http.ResponseWriter, r *http.Request) (string, []byte, error) {
	defer r.Body.Close()
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxFetchBytes+maxSourceImportMultipartOverhead))
	reader, err := r.MultipartReader()
	if err != nil {
		return "", nil, &sourceImportRequestError{detail: "request must be multipart/form-data"}
	}

	var (
		name     string
		data     []byte
		haveName bool
		haveFile bool
	)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, classifySourceImportReadError(err)
		}

		switch part.FormName() {
		case "name":
			if haveName || part.FileName() != "" {
				_ = part.Close()
				return "", nil, &sourceImportRequestError{detail: "multipart form must contain exactly one text name field"}
			}
			value, readErr := io.ReadAll(io.LimitReader(part, int64(maxSourceNameBytes+1)))
			closeErr := part.Close()
			if readErr != nil {
				return "", nil, classifySourceImportReadError(readErr)
			}
			if closeErr != nil {
				return "", nil, classifySourceImportReadError(closeErr)
			}
			if len(value) > maxSourceNameBytes {
				return "", nil, &sourceImportRequestError{detail: fmt.Sprintf("source name exceeds %d bytes", maxSourceNameBytes)}
			}
			name = string(value)
			haveName = true
		case "file":
			if haveFile || part.FileName() == "" {
				_ = part.Close()
				return "", nil, &sourceImportRequestError{detail: "multipart form must contain exactly one file upload field"}
			}
			value, readErr := io.ReadAll(io.LimitReader(part, int64(maxFetchBytes+1)))
			closeErr := part.Close()
			if readErr != nil {
				return "", nil, classifySourceImportReadError(readErr)
			}
			if closeErr != nil {
				return "", nil, classifySourceImportReadError(closeErr)
			}
			if len(value) > maxFetchBytes {
				return "", nil, &sourceImportRequestError{tooLarge: true, detail: fmt.Sprintf("source file exceeds %d byte limit", maxFetchBytes)}
			}
			data = value
			haveFile = true
		default:
			_ = part.Close()
			return "", nil, &sourceImportRequestError{detail: "multipart form may contain only name and file fields"}
		}
	}
	if !haveName || !haveFile {
		return "", nil, &sourceImportRequestError{detail: "multipart form requires one name field and one file field"}
	}
	return name, data, nil
}

func classifySourceImportReadError(err error) error {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) {
		return &sourceImportRequestError{tooLarge: true, detail: fmt.Sprintf("source upload exceeds %d byte limit", maxFetchBytes)}
	}
	return &sourceImportRequestError{detail: "invalid multipart form data"}
}

func writeSourceImportRequestError(w http.ResponseWriter, err error) {
	var requestErr *sourceImportRequestError
	if errors.As(err, &requestErr) && requestErr.tooLarge {
		writeErrCode(w, http.StatusRequestEntityTooLarge, "source_import_too_large", requestErr)
		return
	}
	writeErrCode(w, http.StatusBadRequest, "invalid_source_import", err)
}
