package compute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cps/internal/cloudinit"
	"cps/internal/model"
)

type Hypervisor struct {
	ImagesDir    string
	InstancesDir string
	Logger       *slog.Logger
}

func NewHypervisor(imagesDir, instancesDir string, logger *slog.Logger) *Hypervisor {
	if logger == nil {
		logger = slog.Default()
	}
	return &Hypervisor{ImagesDir: imagesDir, InstancesDir: instancesDir, Logger: logger}
}

func (h *Hypervisor) CheckRuntime() error {
	for _, tool := range []string{"qemu-system-x86_64", "qemu-img"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("required tool %q not found in PATH", tool)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm is unavailable: %w (enable CPU virtualization and grant access to the kvm device)", err)
	}
	if _, err := h.findISOTool(); err != nil {
		return err
	}
	return nil
}

func (h *Hypervisor) findISOTool() (string, error) {
	for _, name := range []string{"genisoimage", "mkisofs", "xorriso"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("no ISO creation tool found; install genisoimage, mkisofs, or xorriso")
}

func (h *Hypervisor) ResolveImage(name string) (model.Image, error) {
	if name == "" || filepath.Base(name) != name || strings.ContainsAny(name, `/\\`) {
		return model.Image{}, fmt.Errorf("invalid image_name %q", name)
	}
	images, err := h.ListImages(context.Background())
	if err != nil {
		return model.Image{}, err
	}
	for _, image := range images {
		if strings.EqualFold(image.Name, name) {
			return image, nil
		}
		for _, alias := range image.Aliases {
			if strings.EqualFold(alias, name) {
				return image, nil
			}
		}
	}
	return model.Image{}, fmt.Errorf("image %q not found; call GET /metadata or GET /images to see supported image names", name)
}

func (h *Hypervisor) Prepare(ctx context.Context, inst *model.Instance, req model.LaunchRequest, sshKey string) error {
	if err := os.MkdirAll(inst.InstanceDir, 0750); err != nil {
		return fmt.Errorf("create instance directory: %w", err)
	}
	image, err := h.ResolveImage(req.ImageName)
	if err != nil {
		return err
	}
	inst.ImageName = image.Name
	if req.SSHUser != "" {
		inst.SSHUser = req.SSHUser
	} else {
		inst.SSHUser = defaultUserForImage(image.Name)
	}

	if _, err := os.Stat(inst.RootDisk); err == nil {
		return fmt.Errorf("root disk already exists at %s; refusing to overwrite it", inst.RootDisk)
	}
	qemuImg, _ := exec.LookPath("qemu-img")
	args := []string{"create", "-f", "qcow2", "-b", image.Path, "-F", "qcow2", inst.RootDisk, fmt.Sprintf("%dG", inst.DiskSizeGB)}
	h.Logger.Info("creating copy-on-write root disk", "instance_id", inst.ID, "image", image.Name, "disk_size_gb", inst.DiskSizeGB, "command", qemuImg, "args", args)
	cmdCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, qemuImg, args...).CombinedOutput()
	h.appendLog(inst.LogFile, fmt.Sprintf("[%s] qemu-img %s\n%s\n", time.Now().Format(time.RFC3339Nano), strings.Join(args, " "), strings.TrimSpace(string(out))))
	if err != nil {
		return fmt.Errorf("qemu-img failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	userData, err := cloudinit.Build(req.UserData, sshKey)
	if err != nil {
		return err
	}
	metadata := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", inst.ID, inst.ID)
	userDataPath := filepath.Join(inst.InstanceDir, "user-data")
	metaDataPath := filepath.Join(inst.InstanceDir, "meta-data")
	if err := os.WriteFile(userDataPath, userData, 0600); err != nil {
		return fmt.Errorf("write cloud-init user-data: %w", err)
	}
	if err := os.WriteFile(metaDataPath, []byte(metadata), 0600); err != nil {
		return fmt.Errorf("write cloud-init meta-data: %w", err)
	}
	isoTool, err := h.findISOTool()
	if err != nil {
		return err
	}
	isoArgs := []string{}
	if filepath.Base(isoTool) == "xorriso" {
		isoArgs = append(isoArgs, "-as", "mkisofs")
	}
	isoArgs = append(isoArgs, "-output", inst.SeedISO, "-volid", "cidata", "-joliet", "-rock", userDataPath, metaDataPath)
	h.Logger.Info("creating cloud-init seed ISO", "instance_id", inst.ID, "tool", isoTool)
	isoCtx, isoCancel := context.WithTimeout(ctx, 60*time.Second)
	defer isoCancel()
	isoOut, err := exec.CommandContext(isoCtx, isoTool, isoArgs...).CombinedOutput()
	h.appendLog(inst.LogFile, fmt.Sprintf("[%s] ISO tool %s %s\n%s\n", time.Now().Format(time.RFC3339Nano), isoTool, strings.Join(isoArgs, " "), strings.TrimSpace(string(isoOut))))
	if err != nil {
		return fmt.Errorf("cloud-init ISO creation failed: %w: %s", err, strings.TrimSpace(string(isoOut)))
	}
	h.Logger.Info("instance storage prepared", "instance_id", inst.ID, "root_disk", inst.RootDisk, "seed_iso", inst.SeedISO)
	return nil
}

func (h *Hypervisor) Launch(ctx context.Context, inst *model.Instance, restoreSnapshot string) (int, error) {
	qemu, err := exec.LookPath("qemu-system-x86_64")
	if err != nil {
		return 0, fmt.Errorf("find QEMU: %w", err)
	}
	_ = os.Remove(inst.QMPsocket) // remove only the stale Unix socket path before a fresh launch
	_ = os.Remove(inst.PIDFile)
	args := []string{
		"-name", inst.ID,
		"-daemonize", "-pidfile", inst.PIDFile,
		"-enable-kvm", "-cpu", "host",
		"-m", strconv.Itoa(inst.MemoryMB), "-smp", strconv.Itoa(inst.VCPUs),
		"-display", "none", "-serial", "file:" + inst.SerialLogFile,
		"-monitor", "none", "-qmp", "unix:" + inst.QMPsocket + ",server=on,wait=off",
		"-d", "guest_errors", "-D", inst.DebugLogFile,
		"-drive", "file=" + inst.RootDisk + ",format=qcow2,if=virtio",
		"-drive", "file=" + inst.SeedISO + ",format=raw,if=virtio,readonly=on",
		"-netdev", fmt.Sprintf("user,id=net0,hostfwd=tcp:%s:%d-:22", inst.SSHHost, inst.SSHPort),
		"-device", "virtio-net-pci,netdev=net0",
	}
	if restoreSnapshot != "" {
		args = append(args, "-S")
	}
	h.Logger.Info("launching QEMU", "instance_id", inst.ID, "vcpus", inst.VCPUs, "memory_mb", inst.MemoryMB, "ssh_host", inst.SSHHost, "ssh_port", inst.SSHPort, "args", args)
	launchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(launchCtx, qemu, args...).CombinedOutput()
	h.appendLog(inst.LogFile, fmt.Sprintf("[%s] qemu launch: %s %s\n%s\n", time.Now().Format(time.RFC3339Nano), qemu, strings.Join(args, " "), strings.TrimSpace(string(out))))
	if err != nil {
		return 0, fmt.Errorf("QEMU launch failed: %w: %s", err, strings.TrimSpace(string(out)))
	}

	deadline := time.Now().Add(20 * time.Second)
	pid := 0
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(inst.PIDFile); readErr == nil {
			if parsed, parseErr := strconv.Atoi(strings.TrimSpace(string(data))); parseErr == nil {
				pid = parsed
				break
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	if pid == 0 {
		return 0, fmt.Errorf("QEMU returned successfully but PID file %s was not created", inst.PIDFile)
	}
	inst.PID = pid
	for time.Now().Before(deadline) {
		statusCtx, statusCancel := context.WithTimeout(ctx, 2*time.Second)
		status, statusErr := qmpStatus(statusCtx, inst.QMPsocket)
		statusCancel()
		if statusErr == nil {
			if restoreSnapshot != "" {
				if _, err := qmpHuman(ctx, inst.QMPsocket, "loadvm "+restoreSnapshot); err != nil {
					_ = h.ForceStop(inst)
					return 0, fmt.Errorf("restore hibernated snapshot %s: %w", restoreSnapshot, err)
				}
				if _, err := qmpHuman(ctx, inst.QMPsocket, "delvm "+restoreSnapshot); err != nil {
					h.Logger.Warn("could not remove restored hibernation snapshot", "instance_id", inst.ID, "snapshot", restoreSnapshot, "error", err)
				}
				if _, err := qmpExecute(ctx, inst.QMPsocket, "cont", nil); err != nil {
					_ = h.ForceStop(inst)
					return 0, fmt.Errorf("resume restored VM: %w", err)
				}
			}
			h.Logger.Info("QEMU is controllable through QMP", "instance_id", inst.ID, "pid", pid, "qmp_status", status)
			return pid, nil
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	_ = h.ForceStop(inst)
	return 0, fmt.Errorf("QEMU process %d started but QMP socket %s did not become ready", pid, inst.QMPsocket)
}

func (h *Hypervisor) GracefulStop(ctx context.Context, inst *model.Instance) error {
	if _, err := qmpExecute(ctx, inst.QMPsocket, "system_powerdown", nil); err != nil {
		return fmt.Errorf("request guest ACPI shutdown: %w", err)
	}
	h.Logger.Info("graceful guest shutdown requested", "instance_id", inst.ID, "pid", inst.PID)
	return nil
}

func (h *Hypervisor) Pause(ctx context.Context, inst *model.Instance) error {
	if _, err := qmpExecute(ctx, inst.QMPsocket, "stop", nil); err != nil {
		return fmt.Errorf("pause VM: %w", err)
	}
	h.Logger.Info("VM paused through QMP", "instance_id", inst.ID)
	return nil
}

func (h *Hypervisor) Resume(ctx context.Context, inst *model.Instance) error {
	if _, err := qmpExecute(ctx, inst.QMPsocket, "cont", nil); err != nil {
		return fmt.Errorf("resume VM: %w", err)
	}
	h.Logger.Info("VM resumed through QMP", "instance_id", inst.ID)
	return nil
}

func (h *Hypervisor) Hibernate(ctx context.Context, inst *model.Instance, snapshot string) error {
	if _, err := qmpHuman(ctx, inst.QMPsocket, "savevm "+snapshot); err != nil {
		return fmt.Errorf("save VM memory/device state to QCOW2 snapshot: %w", err)
	}
	h.Logger.Info("VM state snapshot saved", "instance_id", inst.ID, "snapshot", snapshot)
	return h.FinishHibernate(ctx, inst)
}

// FinishHibernate exits QEMU after a snapshot has already been verified. It is
// separate so startup reconciliation can finish an interrupted hibernate without
// trying to create a duplicate internal snapshot.
func (h *Hypervisor) FinishHibernate(ctx context.Context, inst *model.Instance) error {
	if _, err := qmpExecute(ctx, inst.QMPsocket, "quit", nil); err != nil {
		// QMP may close its socket before sending a response for quit. Verify exit below.
		h.Logger.Debug("QMP quit returned an error while closing", "instance_id", inst.ID, "error", err)
	}
	if err := h.waitForExit(inst, 15*time.Second); err != nil {
		return fmt.Errorf("snapshot was saved but QEMU did not exit: %w", err)
	}
	return nil
}

func (h *Hypervisor) ForceStop(inst *model.Instance) error {
	pid := h.FindPID(inst)
	if pid == 0 {
		return nil
	}
	if !h.IsProcessForInstance(pid, inst) {
		return fmt.Errorf("refusing to signal PID %d because it does not match instance %s", pid, inst.ID)
	}
	h.Logger.Warn("sending SIGKILL to QEMU", "instance_id", inst.ID, "pid", pid)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGKILL QEMU PID %d: %w", pid, err)
	}
	return h.waitForExit(inst, 10*time.Second)
}

func (h *Hypervisor) FindPID(inst *model.Instance) int {
	if inst.PID > 0 && h.IsProcessForInstance(inst.PID, inst) {
		return inst.PID
	}
	if inst.PIDFile != "" {
		data, err := os.ReadFile(inst.PIDFile)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 && h.IsProcessForInstance(pid, inst) {
				return pid
			}
		}
	}
	// Recover if SQLite and the PID file are stale but QEMU is still alive. Match
	// both the executable and this VM's unique root-disk path before trusting a PID.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err == nil && pid > 1 && h.IsProcessForInstance(pid, inst) {
			return pid
		}
	}
	return 0
}

func (h *Hypervisor) IsProcessForInstance(pid int, inst *model.Instance) bool {
	if pid <= 1 {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	if !strings.Contains(cmdline, "qemu-system-x86_64") {
		return false
	}
	if inst.RootDisk != "" && !strings.Contains(cmdline, inst.RootDisk) {
		return false
	}
	if inst.QMPsocket != "" && !strings.Contains(cmdline, inst.QMPsocket) {
		return false
	}
	return true
}

func (h *Hypervisor) Inspect(ctx context.Context, inst *model.Instance) (pid int, qmpState string, live bool, err error) {
	pid = h.FindPID(inst)
	if pid == 0 {
		return 0, "", false, nil
	}
	live = true
	if inst.QMPsocket == "" {
		return pid, "legacy-no-qmp", true, nil
	}
	status, qmpErr := qmpStatus(ctx, inst.QMPsocket)
	if qmpErr != nil {
		return pid, "qmp-unavailable", true, qmpErr
	}
	return pid, status, live, nil
}

func (h *Hypervisor) SnapshotExists(ctx context.Context, inst *model.Instance, snapshot string) (bool, error) {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return false, err
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, qemuImg, "snapshot", "-l", inst.RootDisk).CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("list QCOW2 snapshots: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, snapshot) {
			return true, nil
		}
	}
	return false, nil
}

func (h *Hypervisor) RemoveInstanceFiles(inst *model.Instance) error {
	if inst.InstanceDir == "" {
		return nil
	}
	base, err := filepath.Abs(h.InstancesDir)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(inst.InstanceDir)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(base, dir)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("refusing to remove path outside instances directory: %s", dir)
	}
	h.Logger.Info("removing instance storage", "instance_id", inst.ID, "path", dir)
	return os.RemoveAll(dir)
}

func (h *Hypervisor) waitForExit(inst *model.Instance, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.FindPID(inst) == 0 {
			_ = os.Remove(inst.QMPsocket)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("QEMU PID %d is still alive after %s", inst.PID, timeout)
}

func (h *Hypervisor) appendLog(path, message string) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		h.Logger.Error("cannot write instance operation log", "path", path, "error", err)
		return
	}
	defer f.Close()
	_, _ = f.WriteString(message)
}

func defaultUserForImage(image string) string {
	lower := strings.ToLower(image)
	switch {
	case strings.Contains(lower, "alpine"):
		return "alpine"
	case strings.Contains(lower, "fedora"):
		return "fedora"
	case strings.Contains(lower, "ubuntu"):
		return "ubuntu"
	case strings.Contains(lower, "debian"):
		return "debian"
	default:
		return "cloud-user"
	}
}

func HostArchitecture() string { return runtime.GOARCH }

// ResizeDisk grows a stopped instance's QCOW2 overlay. It deliberately refuses
// shrinking because reducing virtual disk size can destroy guest data.
func (h *Hypervisor) ResizeDisk(ctx context.Context, diskPath string, newSizeGB int) error {
	qemuImg, err := exec.LookPath("qemu-img")
	if err != nil {
		return fmt.Errorf("find qemu-img: %w", err)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	args := []string{"resize", diskPath, fmt.Sprintf("%dG", newSizeGB)}
	h.Logger.Info("growing stopped VM disk", "disk_path", diskPath, "new_size_gb", newSizeGB)
	out, err := exec.CommandContext(cmdCtx, qemuImg, args...).CombinedOutput()
	h.appendLog(filepath.Join(filepath.Dir(diskPath), "qemu.log"), fmt.Sprintf("[%s] qemu-img %s\n%s\n", time.Now().Format(time.RFC3339Nano), strings.Join(args, " "), strings.TrimSpace(string(out))))
	if err != nil {
		return fmt.Errorf("qemu-img resize failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
