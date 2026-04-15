package management

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type probeSchedulerSettings struct {
	Enabled             bool `json:"enabled"`
	IntervalMinutes     int  `json:"intervalMinutes"`
	RetryCount          int  `json:"retryCount"`
	MaxRetryWaitSeconds int  `json:"maxRetryWaitSeconds"`
}

func (h *Handler) probeScriptsDir() (string, error) {
	if h == nil {
		return "", fmt.Errorf("handler is nil")
	}
	if h.configFilePath == "" {
		return "", fmt.Errorf("config file path is empty")
	}
	return filepath.Join(filepath.Dir(h.configFilePath), "scripts"), nil
}

func (h *Handler) probeEnvFilePath() (string, error) {
	dir, err := h.probeScriptsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "model-health.env"), nil
}

func readEnvValue(path, key string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false, err
	}
	lines := strings.Split(string(data), "\n")
	prefix := key + "="
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, prefix)), true, nil
		}
	}
	return "", false, nil
}

func readEnvInt(path, key string) (int, bool, error) {
	raw, found, err := readEnvValue(path, key)
	if err != nil || !found {
		return 0, found, err
	}
	value, parseErr := strconv.Atoi(strings.TrimSpace(raw))
	if parseErr != nil {
		return 0, true, parseErr
	}
	return value, true, nil
}

func writeEnvValue(path, key, value string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	prefix := key + "="
	replaced := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, prefix) {
			lines[i] = prefix + value
			replaced = true
			break
		}
	}
	if !replaced {
		if len(lines) > 0 && lines[len(lines)-1] != "" {
			lines = append(lines, prefix+value)
		} else if len(lines) > 0 {
			lines[len(lines)-1] = prefix + value
			lines = append(lines, "")
		} else {
			lines = []string{prefix + value, ""}
		}
	}
	content := strings.Join(lines, "\n")
	return os.WriteFile(path, []byte(content), 0o644)
}

func (h *Handler) readProbeSchedulerSettings() (probeSchedulerSettings, error) {
	envPath, err := h.probeEnvFilePath()
	if err != nil {
		return probeSchedulerSettings{}, err
	}
	seconds, found, err := readEnvInt(envPath, "CHECK_INTERVAL_SEC")
	if err != nil {
		return probeSchedulerSettings{}, err
	}
	if !found || seconds <= 0 {
		seconds = 3600
	}

	retryCount := 0
	if h != nil && h.cfg != nil && h.cfg.RequestRetry >= 0 {
		retryCount = h.cfg.RequestRetry
	}
	if value, ok, errRead := readEnvInt(envPath, "PROBE_RETRY_COUNT"); errRead == nil && ok {
		retryCount = value
	}
	if retryCount < 0 {
		retryCount = 0
	}

	maxRetryWaitSeconds := 0
	if h != nil && h.cfg != nil && h.cfg.MaxRetryInterval >= 0 {
		maxRetryWaitSeconds = h.cfg.MaxRetryInterval
	}
	if value, ok, errRead := readEnvInt(envPath, "PROBE_MAX_RETRY_WAIT_SEC"); errRead == nil && ok {
		maxRetryWaitSeconds = value
	}
	if maxRetryWaitSeconds < 0 {
		maxRetryWaitSeconds = 0
	}

	enabled, err := h.isProbeSchedulerRunning()
	if err != nil {
		return probeSchedulerSettings{}, err
	}
	minutes := seconds / 60
	if minutes < 1 {
		minutes = 1
	}
	return probeSchedulerSettings{
		Enabled:             enabled,
		IntervalMinutes:     minutes,
		RetryCount:          retryCount,
		MaxRetryWaitSeconds: maxRetryWaitSeconds,
	}, nil
}

func (h *Handler) runProbeScript(scriptName string) error {
	scriptsDir, err := h.probeScriptsDir()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(scriptsDir, scriptName))
	cmd.Dir = filepath.Dir(scriptsDir)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s timed out", scriptName)
	}
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s failed: %s", scriptName, msg)
	}
	return nil
}

func (h *Handler) isProbeSchedulerRunning() (bool, error) {
	scriptsDir, err := h.probeScriptsDir()
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", filepath.Join(scriptsDir, "model-health-status.sh"))
	cmd.Dir = filepath.Dir(scriptsDir)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return false, fmt.Errorf("model-health-status.sh timed out")
	}
	if err != nil {
		msg := strings.TrimSpace(string(output))
		if msg == "" {
			msg = err.Error()
		}
		return false, fmt.Errorf("model-health-status.sh failed: %s", msg)
	}
	return strings.Contains(string(output), "scheduler: running"), nil
}

func (h *Handler) GetProbeScheduler(c *gin.Context) {
	settings, err := h.readProbeSchedulerSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, settings)
}

func (h *Handler) PutProbeScheduler(c *gin.Context) {
	var body struct {
		Enabled             *bool `json:"enabled"`
		IntervalMinutes     *int  `json:"intervalMinutes"`
		RetryCount          *int  `json:"retryCount"`
		MaxRetryWaitSeconds *int  `json:"maxRetryWaitSeconds"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || (body.Enabled == nil && body.IntervalMinutes == nil && body.RetryCount == nil && body.MaxRetryWaitSeconds == nil) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	envPath, err := h.probeEnvFilePath()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	wasRunning, err := h.isProbeSchedulerRunning()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	intervalChanged := false
	if body.IntervalMinutes != nil {
		minutes := *body.IntervalMinutes
		if minutes < 1 {
			minutes = 1
		}
		if err := writeEnvValue(envPath, "CHECK_INTERVAL_SEC", strconv.Itoa(minutes*60)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update model-health.env: %v", err)})
			return
		}
		intervalChanged = true
	}
	if body.RetryCount != nil {
		retryCount := *body.RetryCount
		if retryCount < 0 {
			retryCount = 0
		}
		if err := writeEnvValue(envPath, "PROBE_RETRY_COUNT", strconv.Itoa(retryCount)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update model-health.env: %v", err)})
			return
		}
	}
	if body.MaxRetryWaitSeconds != nil {
		maxRetryWaitSeconds := *body.MaxRetryWaitSeconds
		if maxRetryWaitSeconds < 0 {
			maxRetryWaitSeconds = 0
		}
		if err := writeEnvValue(envPath, "PROBE_MAX_RETRY_WAIT_SEC", strconv.Itoa(maxRetryWaitSeconds)); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": fmt.Sprintf("failed to update model-health.env: %v", err)})
			return
		}
	}

	targetEnabled := wasRunning
	if body.Enabled != nil {
		targetEnabled = *body.Enabled
	}

	if targetEnabled {
		if wasRunning && intervalChanged {
			if err := h.runProbeScript("model-health-stop.sh"); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			wasRunning = false
		}
		if !wasRunning {
			if err := h.runProbeScript("model-health-start.sh"); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
		}
	} else if wasRunning {
		if err := h.runProbeScript("model-health-stop.sh"); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}

	settings, err := h.readProbeSchedulerSettings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, settings)
}
