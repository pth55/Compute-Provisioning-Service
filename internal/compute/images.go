package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"cps/internal/model"
)

func (h *Hypervisor) ListImages(ctx context.Context) ([]model.Image, error) {
	entries, err := os.ReadDir(h.ImagesDir)
	if err != nil {
		return nil, fmt.Errorf("read images directory %s: %w", h.ImagesDir, err)
	}
	qemuImg, qemuErr := exec.LookPath("qemu-img")
	images := make([]model.Image, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".qcow2") {
			continue
		}
		fullPath := filepath.Join(h.ImagesDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			h.Logger.Warn("cannot stat base image", "path", fullPath, "error", err)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		image := model.Image{Name: name, Path: fullPath, Format: "qcow2", ActualSizeGB: float64(info.Size()) / (1024 * 1024 * 1024), Readable: true}
		image.Aliases = imageAliases(name)
		if qemuErr == nil {
			h.Logger.Debug("inspecting base image", "name", name, "path", fullPath)
			cmdCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			out, infoErr := exec.CommandContext(cmdCtx, qemuImg, "info", "--output=json", fullPath).CombinedOutput()
			cancel()
			if infoErr != nil {
				image.Readable = false
				image.Error = strings.TrimSpace(string(out))
			} else {
				var parsed struct {
					Format      string `json:"format"`
					VirtualSize int64  `json:"virtual-size"`
					ActualSize  int64  `json:"actual-size"`
				}
				if err := json.Unmarshal(out, &parsed); err != nil {
					image.Readable = false
					image.Error = err.Error()
				} else {
					if parsed.Format != "" {
						image.Format = parsed.Format
					}
					if parsed.VirtualSize > 0 {
						image.VirtualSizeGB = math.Round((float64(parsed.VirtualSize)/1024/1024/1024)*100) / 100
					}
					if parsed.ActualSize > 0 {
						image.ActualSizeGB = math.Round((float64(parsed.ActualSize)/1024/1024/1024)*100) / 100
					}
				}
			}
		} else {
			image.Readable = false
			image.Error = "qemu-img was not found in PATH"
		}
		images = append(images, image)
	}
	return images, nil
}

func imageAliases(name string) []string {
	lower := strings.ToLower(name)
	aliases := make([]string, 0, 1)
	switch {
	case strings.Contains(lower, "alpine"):
		if !strings.EqualFold(name, "alpine-cloud") {
			aliases = append(aliases, "alpine-cloud")
		}
	case strings.Contains(lower, "fedora-cloud-base-generic") || strings.Contains(lower, "fedora-cloud"):
		if !strings.EqualFold(name, "fedora-cloud") {
			aliases = append(aliases, "fedora-cloud")
		}
	case strings.Contains(lower, "ubuntu"):
		if !strings.EqualFold(name, "ubuntu-cloud") {
			aliases = append(aliases, "ubuntu-cloud")
		}
	case strings.Contains(lower, "debian"):
		if !strings.EqualFold(name, "debian-cloud") {
			aliases = append(aliases, "debian-cloud")
		}
	}
	return aliases
}
