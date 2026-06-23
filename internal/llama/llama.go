// Package llama manages a llama.cpp llama-server child process as the
// node's pinned inference runtime (whitepaper §5.2 profile cuda-consumer).
package llama

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"alien.org/fleet/internal/childproc"
)

type Config struct {
	Bin       string   `json:"bin"`
	ModelPath string   `json:"model_path"`
	Port      int      `json:"port"`
	CtxLen    int      `json:"ctx_len"`
	GPUs      string   `json:"gpus"` // CUDA_VISIBLE_DEVICES value, e.g. "1,2"
	NParallel int      `json:"n_parallel"`
	ExtraArgs []string `json:"extra_args"`
	LogPath   string   `json:"log_path"`
}

type Server struct {
	cfg Config
	cmd *exec.Cmd
}

func New(cfg Config) *Server {
	if cfg.CtxLen == 0 {
		cfg.CtxLen = 8192
	}
	if cfg.NParallel == 0 {
		cfg.NParallel = 2
	}
	return &Server{cfg: cfg}
}

func (s *Server) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port) }

// BundleHash returns the SHA-256 of the model artifact (content addressing,
// AX5). Files are cached beside the artifact keyed on size+mtime. Directories
// are hashed as a stable manifest plus file contents and cached inside the dir.
// Non-local identifiers are hashed as symbolic model ids; use local paths when
// exact artifact identity matters.
func BundleHash(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if os.IsNotExist(err) {
		h := sha256.Sum256([]byte("model-id:" + path))
		return h[:], nil
	}
	if st.IsDir() {
		return dirBundleHash(path)
	}
	cachePath := path + ".sha256"
	cacheKey := fmt.Sprintf("%d-%d", st.Size(), st.ModTime().UnixNano())
	if b, err := os.ReadFile(cachePath); err == nil {
		parts := strings.Fields(string(b))
		if len(parts) == 2 && parts[1] == cacheKey {
			if h, err := hex.DecodeString(parts[0]); err == nil && len(h) == 32 {
				return h, nil
			}
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	sum := h.Sum(nil)
	_ = os.WriteFile(cachePath, []byte(hex.EncodeToString(sum)+" "+cacheKey), 0o644)
	return sum, nil
}

type dirHashEntry struct {
	Rel     string
	Size    int64
	ModTime int64
}

func dirBundleHash(root string) ([]byte, error) {
	var entries []dirHashEntry
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Base(path) == ".fleet_model.sha256" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		entries = append(entries, dirHashEntry{
			Rel:     filepath.ToSlash(rel),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		})
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Rel < entries[j].Rel })
	manifest, _ := json.Marshal(entries)
	manifestHash := sha256.Sum256(manifest)
	cachePath := filepath.Join(root, ".fleet_model.sha256")
	if b, err := os.ReadFile(cachePath); err == nil {
		parts := strings.Fields(string(b))
		if len(parts) == 2 && parts[1] == hex.EncodeToString(manifestHash[:]) {
			if h, err := hex.DecodeString(parts[0]); err == nil && len(h) == 32 {
				return h, nil
			}
		}
	}

	h := sha256.New()
	h.Write([]byte("dir-model-v1\n"))
	h.Write(manifest)
	for _, ent := range entries {
		h.Write([]byte("\n-- " + ent.Rel + "\n"))
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(ent.Rel)))
		if err != nil {
			return nil, err
		}
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	sum := h.Sum(nil)
	_ = os.WriteFile(cachePath, []byte(hex.EncodeToString(sum)+" "+hex.EncodeToString(manifestHash[:])), 0o644)
	return sum, nil
}

// Start launches llama-server pinned to the configured GPUs and waits for
// its health endpoint.
func (s *Server) Start() error {
	args := []string{
		"-m", s.cfg.ModelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.cfg.Port),
		"-ngl", "999",
		"-c", strconv.Itoa(s.cfg.CtxLen),
		"--parallel", strconv.Itoa(s.cfg.NParallel),
		"--no-webui",
	}
	if strings.Contains(s.cfg.GPUs, ",") {
		args = append(args, "--split-mode", "layer")
	}
	args = append(args, s.cfg.ExtraArgs...)
	cmd := exec.Command(s.cfg.Bin, args...)
	binDir := filepath.Dir(s.cfg.Bin)
	cmd.Env = append(os.Environ(),
		"CUDA_VISIBLE_DEVICES="+s.cfg.GPUs,
		"LD_LIBRARY_PATH="+binDir+":"+os.Getenv("LD_LIBRARY_PATH"))
	cmd.SysProcAttr = childproc.SysProcAttr() // die with the node on Linux
	if s.cfg.LogPath != "" {
		lf, err := os.Create(s.cfg.LogPath)
		if err != nil {
			return err
		}
		cmd.Stdout = lf
		cmd.Stderr = lf
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start llama-server: %w", err)
	}
	s.cmd = cmd

	// Wait for readiness (model load can take a while for big bundles).
	deadline := time.Now().Add(5 * time.Minute)
	cli := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if s.cmd.ProcessState != nil {
			return fmt.Errorf("llama-server exited during startup (see %s)", s.cfg.LogPath)
		}
		resp, err := cli.Get(s.BaseURL() + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("llama-server not healthy after 5m (see %s)", s.cfg.LogPath)
}

func (s *Server) Stop() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
}

func (s *Server) Pid() int {
	if s.cmd != nil && s.cmd.Process != nil {
		return s.cmd.Process.Pid
	}
	return 0
}

// Meta is the exact-artifact metadata llama-server extracts from the GGUF.
type Meta struct {
	NParams   uint64
	SizeBytes uint64
	NCtxTrain int
	NEmbd     int
	Quant     string
	Runtime   string
}

var quantRe = regexp.MustCompile(`(?i)\b(i?q[0-9][a-z0-9_]*|f16|bf16|f32)\b`)

// FetchMeta queries the running llama-server for model metadata.
func (s *Server) FetchMeta() (*Meta, error) {
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Get(s.BaseURL() + "/v1/models")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var doc struct {
		Data []struct {
			Meta struct {
				NParams   uint64 `json:"n_params"`
				Size      uint64 `json:"size"`
				NCtxTrain int    `json:"n_ctx_train"`
				NEmbd     int    `json:"n_embd"`
			} `json:"meta"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil || len(doc.Data) == 0 {
		return nil, fmt.Errorf("llama-server /v1/models: %v", err)
	}
	m := &Meta{
		NParams:   doc.Data[0].Meta.NParams,
		SizeBytes: doc.Data[0].Meta.Size,
		NCtxTrain: doc.Data[0].Meta.NCtxTrain,
		NEmbd:     doc.Data[0].Meta.NEmbd,
	}
	if q := quantRe.FindString(filepath.Base(s.cfg.ModelPath)); q != "" {
		m.Quant = strings.ToUpper(q)
	}
	// Runtime version from /props if available.
	if resp2, err := cli.Get(s.BaseURL() + "/props"); err == nil {
		var props struct {
			BuildInfo string `json:"build_info"`
		}
		if json.NewDecoder(resp2.Body).Decode(&props) == nil && props.BuildInfo != "" {
			m.Runtime = "llama.cpp " + props.BuildInfo
		}
		resp2.Body.Close()
	}
	return m, nil
}
