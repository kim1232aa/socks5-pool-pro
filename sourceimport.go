package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const sourceImportDirectoryName = "source_imports"

var (
	ErrSourceImportTooLarge = fmt.Errorf("uploaded source exceeds %d byte limit", maxFetchBytes)
	ErrSourceImportStorage  = errors.New("uploaded source storage failed")
)

type sourceImportStorageError struct {
	cause error
}

func (e *sourceImportStorageError) Error() string { return ErrSourceImportStorage.Error() }
func (e *sourceImportStorageError) Unwrap() error { return e.cause }
func (e *sourceImportStorageError) Is(target error) bool {
	return target == ErrSourceImportStorage
}

// ImportSource validates an uploaded text feed, stores its exact bytes in the
// private data directory, and then publishes the corresponding source config.
// The original client filename is deliberately not accepted or persisted.
func (cs *ConfigStore) ImportSource(name string, reader io.Reader) (Source, int, error) {
	if reader == nil {
		return Source{}, 0, errors.New("uploaded source file is required")
	}

	source, err := validateSourceDefinition(Source{
		Name:   name,
		Kind:   SourceKindUpload,
		Format: FormatTextRegex,
	})
	if err != nil {
		return Source{}, 0, err
	}

	contents, err := io.ReadAll(io.LimitReader(reader, maxFetchBytes+1))
	if err != nil {
		return Source{}, 0, errors.New("read uploaded source failed")
	}
	if len(contents) > maxFetchBytes {
		return Source{}, 0, ErrSourceImportTooLarge
	}
	proxies, err := parseTextRegex(contents)
	if err != nil {
		return Source{}, 0, err
	}
	proxies, err = finalizeFetchedProxies(source, proxies)
	if err != nil {
		return Source{}, 0, err
	}

	source.ID = generateID("src")
	source.Enabled = true
	source.AutoRefreshEnabled = true
	if err := cs.ensureSourceImportDirectory(); err != nil {
		return Source{}, 0, ErrSourceImportStorage
	}
	path, err := cs.importedSourcePath(source.ID)
	if err != nil {
		return Source{}, 0, ErrSourceImportStorage
	}
	if err := writePrivateFileAtomic(path, contents); err != nil {
		return Source{}, 0, ErrSourceImportStorage
	}

	err = cs.mutate(func(cfg *PoolConfig) error {
		if len(cfg.Sources) >= maxConfiguredSources {
			return fmt.Errorf("source limit reached: at most %d sources are allowed", maxConfiguredSources)
		}
		for _, existing := range cfg.Sources {
			if existing.ID == source.ID {
				return errors.New("source id collision")
			}
		}
		cfg.Sources = append(cfg.Sources, source)
		return nil
	})
	if err != nil {
		var persistenceErr *ConfigPersistenceError
		if !errors.As(err, &persistenceErr) {
			_ = cs.removeImportedSourceFile(source.ID)
			return Source{}, 0, err
		}
		if persistenceErr.Outcome != ConfigPersistenceDurabilityUncertain {
			_ = cs.removeImportedSourceFile(source.ID)
		}
		return Source{}, 0, &sourceImportStorageError{cause: err}
	}
	return source, len(proxies), nil
}

// LoadSourceContext dispatches source loading by persisted kind. Legacy empty
// kinds remain remote feeds; uploaded sources never enter the HTTP fetch path.
func (cs *ConfigStore) LoadSourceContext(ctx context.Context, source Source) ([]Proxy, error) {
	return cs.loadSourceContextWithPicker(ctx, source, nil)
}

func (cs *ConfigStore) loadSourceContextWithPicker(ctx context.Context, source Source, picker func() (Proxy, bool)) ([]Proxy, error) {
	kind := strings.ToLower(strings.TrimSpace(source.Kind))
	if kind == "" {
		kind = SourceKindRemote
	}
	if kind == SourceKindRemote {
		return fetchSourceContextWithPicker(ctx, source, picker)
	}
	if kind != SourceKindUpload {
		return nil, fmt.Errorf("unknown source kind: %q", kind)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := validateSourceDefinition(source); err != nil {
		return nil, err
	}
	if err := cs.ensureSourceImportDirectory(); err != nil {
		return nil, errors.New("uploaded source data is unavailable")
	}
	path, err := cs.importedSourcePath(source.ID)
	if err != nil {
		return nil, errors.New("uploaded source data is unavailable")
	}
	contents, err := readPrivateRegularFile(path, maxFetchBytes)
	if err != nil {
		return nil, errors.New("uploaded source data is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proxies, err := parseTextRegex(contents)
	if err != nil {
		return nil, err
	}
	return finalizeFetchedProxies(source, proxies)
}

func (cs *ConfigStore) LoadSource(source Source) ([]Proxy, error) {
	return cs.LoadSourceContext(context.Background(), source)
}

func (cs *ConfigStore) removeImportedSource(source Source) error {
	if err := cs.removeImportedSourceFile(source.ID); err != nil {
		return ErrSourceImportStorage
	}
	return nil
}

func (cs *ConfigStore) removeImportedSourceFile(id string) error {
	if err := cs.ensureSourceImportDirectory(); err != nil {
		return err
	}
	path, err := cs.importedSourcePath(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncPrivateFileDirectory(filepath.Dir(path))
}

func (cs *ConfigStore) importedSourcePath(id string) (string, error) {
	if !validImportedSourceID(id) {
		return "", errors.New("invalid uploaded source id")
	}
	dir, err := cs.sourceImportDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id), nil
}

func (cs *ConfigStore) sourceImportDirectory() (string, error) {
	dir := cs.importDir
	if dir == "" && cs.path != "" {
		dir = filepath.Join(filepath.Dir(cs.path), sourceImportDirectoryName)
	}
	if dir == "" {
		return "", errors.New("uploaded source directory is unavailable")
	}
	return dir, nil
}

func (cs *ConfigStore) ensureSourceImportDirectory() error {
	dir, err := cs.sourceImportDirectory()
	if err != nil {
		return err
	}
	return secureSourceImportDirectory(dir)
}

func secureSourceImportDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("source import location is not a directory")
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	openedInfo, err := directory.Stat()
	if err != nil {
		_ = directory.Close()
		return err
	}
	if !openedInfo.IsDir() || !os.SameFile(info, openedInfo) {
		_ = directory.Close()
		return errors.New("source import directory changed while opening")
	}
	if err := directory.Chmod(0o700); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

func validImportedSourceID(id string) bool {
	if !strings.HasPrefix(id, "src-") || len(id) > maxConfigValueBytes {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' {
			return false
		}
	}
	return true
}
