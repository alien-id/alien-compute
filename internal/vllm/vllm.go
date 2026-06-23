// Package vllm manages a vLLM OpenAI-compatible server child process as a
// Fleet node inference runtime.
package vllm

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"alien.org/fleet/internal/childproc"
	"alien.org/fleet/internal/llama"
)

type Config struct {
	Bin       string   `json:"bin"`
	ModelPath string   `json:"model_path"`
	ModelName string   `json:"model_name"`
	Port      int      `json:"port"`
	CtxLen    int      `json:"ctx_len"`
	GPUs      string   `json:"gpus"` // CUDA_VISIBLE_DEVICES value, e.g. "0,1,2,3"
	ExtraArgs []string `json:"extra_args"`
	LogPath   string   `json:"log_path"`
}

type Server struct {
	cfg Config
	cmd *exec.Cmd
}

func New(cfg Config) *Server {
	if cfg.Bin == "" {
		cfg.Bin = "vllm"
	}
	if cfg.Port == 0 {
		cfg.Port = 8000
	}
	return &Server{cfg: cfg}
}

func (s *Server) BaseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", s.cfg.Port) }

// Start launches `vllm serve` pinned to the configured GPUs and waits for
// vLLM's OpenAI-compatible health endpoint.
func (s *Server) Start() error {
	args := []string{
		"serve", s.cfg.ModelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(s.cfg.Port),
	}
	if s.cfg.ModelName != "" && !hasArg(s.cfg.ExtraArgs, "--served-model-name") {
		args = append(args, "--served-model-name", s.cfg.ModelName)
	}
	if s.cfg.CtxLen > 0 && !hasArg(s.cfg.ExtraArgs, "--max-model-len") {
		args = append(args, "--max-model-len", strconv.Itoa(s.cfg.CtxLen))
	}
	if tp := gpuCount(s.cfg.GPUs); tp > 1 && !hasArg(s.cfg.ExtraArgs, "--tensor-parallel-size") {
		args = append(args, "--tensor-parallel-size", strconv.Itoa(tp))
	}
	args = append(args, s.cfg.ExtraArgs...)

	cmd := exec.Command(s.cfg.Bin, args...)
	cmd.Env = append(os.Environ(), "CUDA_VISIBLE_DEVICES="+s.cfg.GPUs)
	cmd.SysProcAttr = childproc.SysProcAttr()
	if s.cfg.LogPath != "" {
		lf, err := os.Create(s.cfg.LogPath)
		if err != nil {
			return err
		}
		cmd.Stdout = lf
		cmd.Stderr = lf
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start vllm: %w", err)
	}
	s.cmd = cmd

	deadline := time.Now().Add(20 * time.Minute)
	cli := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		if s.cmd.ProcessState != nil {
			return fmt.Errorf("vllm exited during startup (see %s)", s.cfg.LogPath)
		}
		resp, err := cli.Get(s.BaseURL() + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("vllm not healthy after 20m (see %s)", s.cfg.LogPath)
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

var quantRe = regexp.MustCompile(`(?i)\b(fp8|int8|int4|awq|gptq|bnb|f16|bf16|f32)\b`)

// FetchMeta returns the metadata vLLM exposes, plus local artifact size when
// ModelPath points at a local file or directory.
func (s *Server) FetchMeta() (*llama.Meta, error) {
	m := &llama.Meta{
		SizeBytes: localSize(s.cfg.ModelPath),
		NCtxTrain: s.cfg.CtxLen,
		Runtime:   "vLLM",
	}
	if q := quantRe.FindString(filepath.Base(s.cfg.ModelPath)); q != "" {
		m.Quant = strings.ToUpper(q)
	}

	cli := &http.Client{Timeout: 5 * time.Second}
	if resp, err := cli.Get(s.BaseURL() + "/v1/models"); err == nil {
		var doc struct {
			Data []struct {
				ID          string `json:"id"`
				MaxModelLen int    `json:"max_model_len"`
			} `json:"data"`
		}
		if json.NewDecoder(resp.Body).Decode(&doc) == nil && len(doc.Data) > 0 {
			if doc.Data[0].MaxModelLen > 0 {
				m.NCtxTrain = doc.Data[0].MaxModelLen
			}
		}
		resp.Body.Close()
	}
	if resp, err := cli.Get(s.BaseURL() + "/version"); err == nil {
		var doc struct {
			Version string `json:"version"`
		}
		if json.NewDecoder(resp.Body).Decode(&doc) == nil && doc.Version != "" {
			m.Runtime = "vLLM " + doc.Version
		}
		resp.Body.Close()
	}
	return m, nil
}

func hasArg(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
	}
	return false
}

func gpuCount(gpus string) int {
	n := 0
	for _, part := range strings.Split(gpus, ",") {
		if strings.TrimSpace(part) != "" {
			n++
		}
	}
	return n
}

func localSize(path string) uint64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if !st.IsDir() {
		return uint64(st.Size())
	}
	var total uint64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Size() > 0 {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}
