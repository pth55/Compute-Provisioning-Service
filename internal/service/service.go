package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"cps/internal/compute"
	"cps/internal/db"
	"cps/internal/model"
)

type Service struct {
	Store        *db.Store
	Hypervisor   *compute.Hypervisor
	DataDir      string
	ImagesDir    string
	InstancesDir string
	NodeID       string
	NodeName     string
	Region       string
	Zone         string
	Logger       *slog.Logger
	createMu     sync.Mutex
	locks        sync.Map
}

func New(store *db.Store, hypervisor *compute.Hypervisor, dataDir string, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	region := os.Getenv("CPS_REGION")
	if region == "" {
		region = "local"
	}
	zone := os.Getenv("CPS_ZONE")
	if zone == "" {
		zone = region + "-1a"
	}
	return &Service{Store: store, Hypervisor: hypervisor, DataDir: dataDir, ImagesDir: hypervisor.ImagesDir, InstancesDir: hypervisor.InstancesDir,
		NodeID: host, NodeName: host, Region: region, Zone: zone, Logger: logger}
}

func (s *Service) instanceLock(id string) *sync.Mutex {
	lock, _ := s.locks.LoadOrStore(id, &sync.Mutex{})
	return lock.(*sync.Mutex)
}

func (s *Service) Create(ctx context.Context, req model.LaunchRequest) (*model.Instance, error) {
	s.createMu.Lock()
	defer s.createMu.Unlock()
	if err := s.Hypervisor.CheckRuntime(); err != nil {
		return nil, err
	}
	if req.VCPUs == 0 {
		req.VCPUs = 1
	}
	if req.MemoryMB == 0 {
		req.MemoryMB = 512
	}
	if req.DiskSizeGB == 0 {
		req.DiskSizeGB = 10
	}
	if req.VCPUs < 1 || req.VCPUs > 256 {
		return nil, fmt.Errorf("vcpus must be between 1 and 256")
	}
	if req.MemoryMB < 128 || req.MemoryMB > 1048576 {
		return nil, fmt.Errorf("memory_mb must be between 128 and 1048576")
	}
	if req.DiskSizeGB < 1 || req.DiskSizeGB > 16384 {
		return nil, fmt.Errorf("disk_size_gb must be between 1 and 16384")
	}
	if len(req.UserData) > 256*1024 {
		return nil, fmt.Errorf("user_data must not exceed 256 KiB")
	}

	images, err := s.Hypervisor.ListImages(ctx)
	if err != nil {
		return nil, err
	}
	if len(images) == 0 {
		return nil, fmt.Errorf("no .qcow2 images found in %s", s.ImagesDir)
	}
	if req.ImageName == "" {
		for _, image := range images {
			for _, alias := range image.Aliases {
				if alias == "alpine-cloud" {
					req.ImageName = alias
				}
			}
		}
		if req.ImageName == "" {
			for _, image := range images {
				if image.Readable {
					req.ImageName = image.Name
					break
				}
			}
		}
		if req.ImageName == "" {
			return nil, fmt.Errorf("no readable QCOW2 images are available")
		}
	}
	image, err := s.Hypervisor.ResolveImage(req.ImageName)
	if err != nil {
		return nil, err
	}
	if !image.Readable {
		return nil, fmt.Errorf("image %q is not readable by qemu-img: %s", req.ImageName, image.Error)
	}
	req.ImageName = image.Name

	sshKey := strings.TrimSpace(req.SSHPublicKey)
	if sshKey == "" {
		sshKey = s.defaultPublicKey()
	}
	if sshKey == "" {
		return nil, fmt.Errorf("ssh_public_key is required, or create ~/.ssh/id_ed25519.pub on the machine running CPS")
	}
	if err := validatePublicKey(sshKey); err != nil {
		return nil, err
	}
	req.SSHPublicKey = sshKey
	if req.SSHUser != "" {
		if !regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`).MatchString(req.SSHUser) {
			return nil, fmt.Errorf("ssh_user must be a valid Linux username")
		}
	}

	meta, err := s.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	if uint64(req.MemoryMB) > meta.AllocatableMemoryMB {
		return nil, fmt.Errorf("insufficient allocatable memory: requested %d MB, available %d MB after CPS reservations", req.MemoryMB, meta.AllocatableMemoryMB)
	}
	if req.VCPUs > meta.AllocatableVCPUs {
		return nil, fmt.Errorf("CPU allocation limit exceeded: requested %d vCPUs, allocatable %d vCPUs (4:1 vCPU overcommit policy)", req.VCPUs, meta.AllocatableVCPUs)
	}
	// QCOW2 disks are sparse, but CPS reserves virtual disk capacity as well as
	// physical free space so multiple sparse disks cannot silently overcommit a host.
	if uint64(req.DiskSizeGB) > meta.AllocatableDiskGB {
		return nil, fmt.Errorf("disk request exceeds safe host disk headroom: requested %d GB, allocatable %d GB after existing VM reservations and safety reserve", req.DiskSizeGB, meta.AllocatableDiskGB)
	}

	id, err := generateID()
	if err != nil {
		return nil, err
	}
	port, err := s.allocatePort(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	instDir := filepath.Join(s.InstancesDir, id)
	inst := &model.Instance{
		ID: id, PID: 0, Status: model.StateCreating, SSHHost: "127.0.0.1", SSHPort: port,
		VCPUs: req.VCPUs, MemoryMB: req.MemoryMB, DiskSizeGB: req.DiskSizeGB, ImageName: image.Name,
		RootDisk: filepath.Join(instDir, "root.qcow2"), SeedISO: filepath.Join(instDir, "cidata.iso"), InstanceDir: instDir,
		QMPsocket: filepath.Join(instDir, "qmp.sock"), PIDFile: filepath.Join(instDir, "qemu.pid"), LogFile: filepath.Join(instDir, "qemu.log"),
		SerialLogFile: filepath.Join(instDir, "serial.log"), DebugLogFile: filepath.Join(instDir, "qemu-debug.log"),
		NodeID: s.NodeID, NodeName: s.NodeName, Region: s.Region, Zone: s.Zone, CreatedAt: now, UpdatedAt: now,
	}
	if req.SSHUser != "" {
		inst.SSHUser = req.SSHUser
	} else {
		inst.SSHUser = defaultSSHUser(image.Name)
	}
	if err := s.Store.CreateInstance(ctx, inst, fmt.Sprintf("Provisioning accepted: image=%s vcpus=%d memory_mb=%d disk_size_gb=%d ssh_port=%d", inst.ImageName, inst.VCPUs, inst.MemoryMB, inst.DiskSizeGB, inst.SSHPort)); err != nil {
		return nil, err
	}
	s.Logger.Info("instance provisioning accepted", "instance_id", id, "image", inst.ImageName, "vcpus", inst.VCPUs, "memory_mb", inst.MemoryMB, "disk_size_gb", inst.DiskSizeGB, "ssh_port", port, "node", inst.NodeName)
	go s.provision(id, req)
	return inst, nil
}

func (s *Service) provision(id string, req model.LaunchRequest) {
	lock := s.instanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	inst, err := s.Store.GetInstance(ctx, id)
	if err != nil {
		s.Logger.Error("cannot reload instance for provisioning", "instance_id", id, "error", err)
		return
	}
	if inst.Status != model.StateCreating {
		return
	}
	s.emit(ctx, id, "INFO", "provisioning", "PROVISIONING_STARTED", fmt.Sprintf("starting VM provisioning on node %s", inst.NodeName))
	s.Logger.Info("provisioning step started", "instance_id", id, "step", "prepare_disk_and_cloud_init")
	if err := s.Hypervisor.Prepare(ctx, inst, req, req.SSHPublicKey); err != nil {
		s.failInstance(ctx, inst, fmt.Errorf("prepare VM: %w", err))
		return
	}
	// Persist any resolved image/user metadata before QEMU starts, so recovery can
	// find the exact disk and configuration even if the daemon is killed abruptly.
	inst.UpdatedAt = time.Now().UTC()
	if err := s.Store.UpdateInstance(ctx, inst, "INFO", "provisioning", "STORAGE_READY", "QCOW2 overlay and cloud-init seed ISO prepared"); err != nil {
		s.Logger.Error("persist prepared instance metadata", "instance_id", id, "error", err)
		return
	}
	s.Logger.Info("provisioning step started", "instance_id", id, "step", "launch_qemu")
	pid, err := s.Hypervisor.Launch(ctx, inst, "")
	if err != nil {
		s.failInstance(ctx, inst, fmt.Errorf("launch QEMU: %w", err))
		return
	}
	inst.PID = pid
	inst.LastError = ""
	if err := s.transition(ctx, inst, model.StateRunning, "QEMU launched and is reachable through QMP"); err != nil {
		s.Logger.Error("QEMU launched but final state could not be persisted; reconciliation will repair it", "instance_id", id, "pid", pid, "error", err)
		return
	}
	s.Logger.Info("instance is running", "instance_id", id, "pid", pid, "ssh_target", fmt.Sprintf("%s@%s:%d", inst.SSHUser, inst.SSHHost, inst.SSHPort), "root_disk", inst.RootDisk, "qmp_socket", inst.QMPsocket)
}

func (s *Service) failInstance(ctx context.Context, inst *model.Instance, cause error) {
	inst.PID = 0
	inst.LastError = cause.Error()
	if err := s.transition(ctx, inst, model.StateFailed, "provisioning failed: "+cause.Error()); err != nil {
		s.Logger.Error("persist provisioning failure", "instance_id", inst.ID, "cause", cause, "persist_error", err)
	}
	s.Logger.Error("instance provisioning failed", "instance_id", inst.ID, "error", cause, "log_file", inst.LogFile)
}

func (s *Service) GetInstance(ctx context.Context, id string) (*model.Instance, error) {
	return s.Store.GetInstance(ctx, id)
}
func (s *Service) ListInstances(ctx context.Context) ([]model.Instance, error) {
	return s.Store.ListInstances(ctx)
}
func (s *Service) ListEvents(ctx context.Context, instanceID string, limit int) ([]model.Event, error) {
	return s.Store.ListEvents(ctx, instanceID, limit)
}
func (s *Service) ListImages(ctx context.Context) ([]model.Image, error) {
	return s.Hypervisor.ListImages(ctx)
}

func (s *Service) Action(ctx context.Context, id, action string) (*model.Instance, error) {
	lock := s.instanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	inst, err := s.Store.GetInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	oldState := inst.Status
	s.Logger.Info("instance lifecycle action requested", "instance_id", id, "action", action, "state", oldState)
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start":
		if oldState != model.StateStopped && oldState != model.StateHibernated && oldState != model.StateFailed {
			return nil, fmt.Errorf("action start is not valid while instance is %s", oldState)
		}
		if _, err := os.Stat(inst.RootDisk); err != nil {
			return nil, fmt.Errorf("root disk is unavailable; decommission this failed instance and create a new one: %w", err)
		}
		if _, err := os.Stat(inst.SeedISO); err != nil {
			return nil, fmt.Errorf("cloud-init seed ISO is unavailable; decommission this instance and create a new one: %w", err)
		}
		capacity, capacityErr := s.Metadata(ctx)
		if capacityErr != nil {
			return nil, capacityErr
		}
		if uint64(inst.MemoryMB) > capacity.AllocatableMemoryMB {
			return nil, fmt.Errorf("insufficient allocatable memory to start VM: requested %d MB, available %d MB", inst.MemoryMB, capacity.AllocatableMemoryMB)
		}
		if inst.VCPUs > capacity.AllocatableVCPUs {
			return nil, fmt.Errorf("insufficient allocatable vCPUs to start VM: requested %d, available %d", inst.VCPUs, capacity.AllocatableVCPUs)
		}
		if err := s.transition(ctx, inst, model.StateStarting, "start requested; launching QEMU"); err != nil {
			return nil, err
		}
		snapshot := ""
		if oldState == model.StateHibernated || (oldState == model.StateFailed && inst.HibernateSnapshot != "") {
			snapshot = inst.HibernateSnapshot
		}
		pid, launchErr := s.Hypervisor.Launch(ctx, inst, snapshot)
		if launchErr != nil {
			inst.PID = 0
			back := model.StateStopped
			if oldState == model.StateHibernated || (oldState == model.StateFailed && snapshot != "") {
				back = model.StateHibernated
			} else if oldState == model.StateFailed {
				back = model.StateFailed
			}
			inst.LastError = launchErr.Error()
			_ = s.transition(ctx, inst, back, "start failed: "+launchErr.Error())
			return nil, launchErr
		}
		inst.PID = pid
		inst.HibernateSnapshot = ""
		inst.LastError = ""
		inst.StartedAt = time.Now().UTC()
		if err := s.transition(ctx, inst, model.StateRunning, "QEMU launched and QMP reports the VM controllable"); err != nil {
			return nil, err
		}
		return inst, nil
	case "stop":
		if oldState == model.StateStopped || oldState == model.StateHibernated {
			return inst, nil
		}
		if oldState != model.StateRunning && oldState != model.StatePaused {
			return nil, fmt.Errorf("action stop is not valid while instance is %s", oldState)
		}
		if inst.QMPsocket == "" {
			return nil, fmt.Errorf("QMP is unavailable for this legacy VM; force_stop is available, or recreate the VM with the current CPS version")
		}
		if oldState == model.StatePaused {
			if err := s.Hypervisor.Resume(ctx, inst); err != nil {
				return nil, err
			}
		}
		if err := s.transition(ctx, inst, model.StateStopping, "ACPI graceful shutdown requested"); err != nil {
			return nil, err
		}
		if err := s.Hypervisor.GracefulStop(ctx, inst); err != nil {
			inst.LastError = err.Error()
			rollback := oldState
			if oldState == model.StatePaused {
				if pauseErr := s.Hypervisor.Pause(ctx, inst); pauseErr != nil {
					s.Logger.Error("failed to restore paused state after shutdown request failure", "instance_id", id, "error", pauseErr)
				}
				rollback = model.StatePaused
			}
			_ = s.transition(ctx, inst, rollback, "graceful shutdown request failed: "+err.Error())
			return nil, err
		}
		return inst, nil
	case "force_stop":
		if oldState == model.StateStopped || oldState == model.StateHibernated {
			return inst, nil
		}
		if oldState == model.StateDecommissioned || oldState == model.StateDecommissioning {
			return nil, fmt.Errorf("instance is being decommissioned")
		}
		if oldState == model.StateCreating || oldState == model.StateStarting {
			return nil, fmt.Errorf("action force_stop is not valid while instance is %s; wait for provisioning/start to finish or decommission it", oldState)
		}
		if err := s.Hypervisor.ForceStop(inst); err != nil {
			return nil, err
		}
		inst.PID = 0
		inst.LastError = ""
		inst.StoppedAt = time.Now().UTC()
		if err := s.transition(ctx, inst, model.StateStopped, "QEMU force-stopped; guest filesystem may be inconsistent"); err != nil {
			return nil, err
		}
		return inst, nil
	case "pause":
		if oldState != model.StateRunning {
			if oldState == model.StatePaused {
				return inst, nil
			}
			return nil, fmt.Errorf("action pause is not valid while instance is %s", oldState)
		}
		if err := s.Hypervisor.Pause(ctx, inst); err != nil {
			return nil, err
		}
		if err := s.transition(ctx, inst, model.StatePaused, "QEMU stop command accepted; guest is paused in host RAM"); err != nil {
			return nil, err
		}
		return inst, nil
	case "resume":
		if oldState != model.StatePaused {
			if oldState == model.StateRunning {
				return inst, nil
			}
			return nil, fmt.Errorf("action resume is not valid while instance is %s", oldState)
		}
		if err := s.Hypervisor.Resume(ctx, inst); err != nil {
			return nil, err
		}
		inst.LastError = ""
		if err := s.transition(ctx, inst, model.StateRunning, "QEMU cont command accepted; guest execution resumed"); err != nil {
			return nil, err
		}
		return inst, nil
	case "hibernate":
		if oldState != model.StateRunning && oldState != model.StatePaused {
			if oldState == model.StateHibernated {
				return inst, nil
			}
			return nil, fmt.Errorf("action hibernate is not valid while instance is %s", oldState)
		}
		if inst.QMPsocket == "" {
			return nil, fmt.Errorf("QMP is unavailable for this legacy VM; recreate it with the current CPS version before hibernating")
		}
		snapshot := fmt.Sprintf("cps-hibernate-%x", time.Now().UTC().UnixNano())
		inst.HibernateSnapshot = snapshot
		if err := s.transition(ctx, inst, model.StateHibernating, "saving RAM and device state as QCOW2 internal snapshot"); err != nil {
			return nil, err
		}
		// VM state save can be large; a dedicated operation context avoids inheriting
		// a short HTTP client deadline after the API has accepted the request.
		hibernateCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := s.Hypervisor.Hibernate(hibernateCtx, inst, snapshot); err != nil {
			inst.HibernateSnapshot = ""
			inst.LastError = err.Error()
			rollback := oldState
			if rollback == model.StatePaused {
				rollback = model.StatePaused
			} else {
				rollback = model.StateRunning
			}
			_ = s.transition(ctx, inst, rollback, "hibernate failed: "+err.Error())
			return nil, err
		}
		inst.PID = 0
		inst.LastError = ""
		inst.StoppedAt = time.Now().UTC()
		if err := s.transition(ctx, inst, model.StateHibernated, "VM state saved to disk and QEMU process exited"); err != nil {
			return nil, err
		}
		return inst, nil
	default:
		return nil, fmt.Errorf("unsupported action %q; allowed actions: start, stop, force_stop, pause, resume, hibernate", action)
	}
}

func (s *Service) UpdateConfig(ctx context.Context, id string, req model.UpdateInstanceRequest) (*model.Instance, error) {
	lock := s.instanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	inst, err := s.Store.GetInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	if inst.Status != model.StateStopped {
		return nil, fmt.Errorf("configuration can only be changed while instance is STOPPED; current state is %s", inst.Status)
	}
	if req.VCPUs == nil && req.MemoryMB == nil && req.DiskSizeGB == nil {
		return nil, fmt.Errorf("provide at least one of vcpus, memory_mb, or disk_size_gb")
	}
	newCPUs, newMemory, newDisk := inst.VCPUs, inst.MemoryMB, inst.DiskSizeGB
	if req.VCPUs != nil {
		newCPUs = *req.VCPUs
	}
	if req.MemoryMB != nil {
		newMemory = *req.MemoryMB
	}
	if req.DiskSizeGB != nil {
		newDisk = *req.DiskSizeGB
	}
	if newCPUs < 1 || newCPUs > 256 {
		return nil, fmt.Errorf("vcpus must be between 1 and 256")
	}
	if newMemory < 128 || newMemory > 1048576 {
		return nil, fmt.Errorf("memory_mb must be between 128 and 1048576")
	}
	if newDisk < inst.DiskSizeGB {
		return nil, fmt.Errorf("disk shrinking is not supported; requested %d GB is smaller than the current %d GB", newDisk, inst.DiskSizeGB)
	}
	if newDisk < 1 || newDisk > 16384 {
		return nil, fmt.Errorf("disk_size_gb must be between 1 and 16384")
	}

	meta, err := s.Metadata(ctx)
	if err != nil {
		return nil, err
	}
	if req.MemoryMB != nil && uint64(newMemory) > meta.AllocatableMemoryMB {
		return nil, fmt.Errorf("insufficient allocatable memory: requested %d MB, available %d MB", newMemory, meta.AllocatableMemoryMB)
	}
	if req.VCPUs != nil && newCPUs > meta.AllocatableVCPUs {
		return nil, fmt.Errorf("CPU allocation limit exceeded: requested %d vCPUs, allocatable %d", newCPUs, meta.AllocatableVCPUs)
	}
	if newDisk > inst.DiskSizeGB {
		delta := uint64(newDisk - inst.DiskSizeGB)
		if delta > meta.AllocatableDiskGB {
			return nil, fmt.Errorf("insufficient reserved host disk headroom to grow disk by %d GB; allocatable %d GB", delta, meta.AllocatableDiskGB)
		}
		if err := s.Hypervisor.ResizeDisk(ctx, inst.RootDisk, newDisk); err != nil {
			return nil, err
		}
	}
	inst.VCPUs, inst.MemoryMB, inst.DiskSizeGB = newCPUs, newMemory, newDisk
	inst.UpdatedAt = time.Now().UTC()
	inst.LastError = ""
	message := fmt.Sprintf("offline configuration updated: vcpus=%d memory_mb=%d disk_size_gb=%d", newCPUs, newMemory, newDisk)
	if err := s.Store.UpdateInstance(ctx, inst, "INFO", "configuration", "INSTANCE_CONFIG_UPDATED", message); err != nil {
		return nil, err
	}
	s.Logger.Info("instance configuration updated", "instance_id", id, "vcpus", newCPUs, "memory_mb", newMemory, "disk_size_gb", newDisk)
	return inst, nil
}

func (s *Service) Decommission(ctx context.Context, id string) (*model.Instance, error) {
	lock := s.instanceLock(id)
	lock.Lock()
	defer lock.Unlock()
	inst, err := s.Store.GetInstance(ctx, id)
	if err != nil {
		return nil, err
	}
	if inst.Status == model.StateDecommissioned {
		return inst, nil
	}
	if inst.Status != model.StateDecommissioning {
		if err := s.transition(ctx, inst, model.StateDecommissioning, "decommission requested; VM process and local files will be removed"); err != nil {
			return nil, err
		}
	}
	if err := s.Hypervisor.ForceStop(inst); err != nil {
		inst.LastError = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		if persistErr := s.Store.UpdateInstance(ctx, inst, "ERROR", "lifecycle", "DECOMMISSION_PENDING", "waiting for QEMU termination: "+err.Error()); persistErr != nil {
			s.Logger.Error("persist decommission failure", "instance_id", id, "operation_error", err, "persist_error", persistErr)
		}
		return nil, err
	}
	if err := s.Hypervisor.RemoveInstanceFiles(inst); err != nil {
		inst.LastError = err.Error()
		inst.UpdatedAt = time.Now().UTC()
		if persistErr := s.Store.UpdateInstance(ctx, inst, "ERROR", "lifecycle", "DECOMMISSION_CLEANUP_FAILED", "storage cleanup failed: "+err.Error()); persistErr != nil {
			s.Logger.Error("persist decommission cleanup failure", "instance_id", id, "operation_error", err, "persist_error", persistErr)
		}
		return nil, err
	}
	inst.PID = 0
	inst.LastError = ""
	inst.DecommissionedAt = time.Now().UTC()
	if err := s.transition(ctx, inst, model.StateDecommissioned, "VM stopped, local instance files removed, inventory tombstone retained"); err != nil {
		return nil, err
	}
	return inst, nil
}

func (s *Service) transition(ctx context.Context, inst *model.Instance, target model.State, message string) error {
	from := inst.Status
	if from == target {
		return nil
	}
	if !allowedTransition(from, target) {
		return fmt.Errorf("invalid lifecycle transition %s -> %s for instance %s", from, target, inst.ID)
	}
	now := time.Now().UTC()
	inst.Status = target
	inst.UpdatedAt = now
	if target == model.StateRunning {
		inst.StartedAt = now
	}
	if target == model.StateStopped || target == model.StateHibernated {
		inst.StoppedAt = now
	}
	if target == model.StateDecommissioned {
		inst.DecommissionedAt = now
	}
	level := "INFO"
	if target == model.StateFailed || inst.LastError != "" {
		level = "ERROR"
	}
	fullMessage := string(from) + " -> " + string(target) + ": " + message
	if err := s.Store.UpdateInstance(ctx, inst, level, "lifecycle", "STATE_TRANSITION", fullMessage); err != nil {
		return fmt.Errorf("persist lifecycle transition: %w", err)
	}
	if level == "ERROR" {
		s.Logger.Error("instance state transition", "instance_id", inst.ID, "from", from, "to", target, "message", message)
	} else {
		s.Logger.Info("instance state transition", "instance_id", inst.ID, "from", from, "to", target, "message", message)
	}
	return nil
}

func allowedTransition(from, to model.State) bool {
	if to == model.StateDecommissioning && from != model.StateDecommissioned {
		return true
	}
	allowed := map[model.State]map[model.State]bool{
		model.StateCreating:        {model.StateRunning: true, model.StateFailed: true, model.StateUnknown: true},
		model.StateStarting:        {model.StateRunning: true, model.StateStopped: true, model.StateHibernated: true, model.StateFailed: true, model.StateUnknown: true},
		model.StateRunning:         {model.StateStopping: true, model.StatePaused: true, model.StateHibernating: true, model.StateStopped: true, model.StateFailed: true, model.StateUnknown: true},
		model.StatePaused:          {model.StateRunning: true, model.StateStopping: true, model.StateHibernating: true, model.StateStopped: true, model.StateFailed: true, model.StateUnknown: true},
		model.StateStopping:        {model.StateStopped: true, model.StateRunning: true, model.StatePaused: true, model.StateFailed: true, model.StateUnknown: true},
		model.StateStopped:         {model.StateStarting: true, model.StateRunning: true, model.StatePaused: true, model.StateFailed: true, model.StateUnknown: true},
		model.StateHibernating:     {model.StateHibernated: true, model.StateRunning: true, model.StatePaused: true, model.StateFailed: true, model.StateUnknown: true},
		model.StateHibernated:      {model.StateStarting: true, model.StateFailed: true, model.StateUnknown: true},
		model.StateDecommissioning: {model.StateDecommissioned: true},
		model.StateFailed:          {model.StateStarting: true, model.StateRunning: true, model.StatePaused: true, model.StateStopped: true, model.StateUnknown: true},
		model.StateUnknown:         {model.StateRunning: true, model.StatePaused: true, model.StateStopped: true, model.StateFailed: true, model.StateStarting: true, model.StateHibernated: true},
	}
	return allowed[from][to]
}

func (s *Service) Metadata(ctx context.Context) (model.ClusterMetadata, error) {
	images, err := s.Hypervisor.ListImages(ctx)
	if err != nil {
		return model.ClusterMetadata{}, err
	}
	instances, err := s.Store.ListInstances(ctx)
	if err != nil {
		return model.ClusterMetadata{}, err
	}
	hostTotal, hostAvailable := readMemInfo()
	diskTotal, diskAvailable := diskInfo(s.InstancesDir)
	meta := model.ClusterMetadata{NodeID: s.NodeID, NodeName: s.NodeName, Region: s.Region, Zone: s.Zone, OS: runtime.GOOS, Architecture: runtime.GOARCH, HostCPUCount: runtime.NumCPU(), HostMemTotalMB: hostTotal, HostMemAvailableMB: hostAvailable, HostDiskTotalGB: diskTotal, HostDiskAvailableGB: diskAvailable, SupportedImages: make([]string, 0), Images: images, InstanceCountsByState: make(map[string]int)}
	imageNameSet := make(map[string]bool)
	for _, image := range images {
		if image.Readable {
			imageNameSet[image.Name] = true
			for _, alias := range image.Aliases {
				imageNameSet[alias] = true
			}
		}
	}
	for name := range imageNameSet {
		meta.SupportedImages = append(meta.SupportedImages, name)
	}
	sort.Strings(meta.SupportedImages)
	allocatedRAM, allocatedDisk, allocatedCPU := uint64(0), uint64(0), 0
	for _, inst := range instances {
		meta.InstanceCountsByState[string(inst.Status)]++
		if inst.Status != model.StateDecommissioned && inst.DiskSizeGB > 0 {
			allocatedDisk += uint64(inst.DiskSizeGB)
		}
		switch inst.Status {
		case model.StateCreating, model.StateStarting, model.StateRunning, model.StateStopping, model.StatePaused, model.StateUnknown:
			allocatedRAM += uint64(max(inst.MemoryMB, 0))
			allocatedCPU += inst.VCPUs
		}
	}
	meta.AllocatedDiskGB = allocatedDisk
	if meta.HostDiskAvailableGB > 2+allocatedDisk {
		meta.AllocatableDiskGB = meta.HostDiskAvailableGB - 2 - allocatedDisk
	}
	meta.AllocatedMemoryMB = int(minUint64(allocatedRAM, uint64(^uint(0)>>1)))
	meta.AllocatedVCPUs = allocatedCPU
	if meta.HostCPUCount > 0 {
		meta.AllocatableVCPUs = meta.HostCPUCount*4 - allocatedCPU
		if meta.AllocatableVCPUs < 0 {
			meta.AllocatableVCPUs = 0
		}
	}
	const reserveRAM = 512
	maxByTotal := uint64(0)
	if meta.HostMemTotalMB > uint64(reserveRAM)+allocatedRAM {
		maxByTotal = meta.HostMemTotalMB - uint64(reserveRAM) - allocatedRAM
	}
	meta.AllocatableMemoryMB = minUint64(hostAvailable, maxByTotal)
	return meta, nil
}

func (s *Service) Inventory(ctx context.Context) (model.Inventory, error) {
	metadata, err := s.Metadata(ctx)
	if err != nil {
		return model.Inventory{}, err
	}
	instances, err := s.Store.ListInstances(ctx)
	if err != nil {
		return model.Inventory{}, err
	}
	events, err := s.Store.ListEvents(ctx, "", 25)
	if err != nil {
		return model.Inventory{}, err
	}
	return model.Inventory{Metadata: metadata, Instances: instances, Events: events}, nil
}

func (s *Service) Reconcile(ctx context.Context) {
	instances, err := s.Store.ListInstances(ctx)
	if err != nil {
		s.Logger.Error("reconciliation cannot load inventory", "error", err)
		return
	}
	for _, candidate := range instances {
		if ctx.Err() != nil {
			return
		}
		lock := s.instanceLock(candidate.ID)
		if !lock.TryLock() {
			continue
		}
		s.reconcileOne(ctx, &candidate)
		lock.Unlock()
	}
}

func (s *Service) reconcileOne(ctx context.Context, candidate *model.Instance) {
	inst, err := s.hydrateLegacyPaths(ctx, candidate)
	if err != nil {
		s.Logger.Error("reconcile: could not hydrate legacy instance", "instance_id", candidate.ID, "error", err)
		return
	}
	if inst.Status == model.StateDecommissioned {
		return
	}
	if inst.Status == model.StateDecommissioning {
		if err := s.Hypervisor.ForceStop(inst); err != nil {
			inst.LastError = err.Error()
			_ = s.Store.UpdateInstance(ctx, inst, "ERROR", "reconciliation", "DECOMMISSION_PENDING", "waiting for VM termination: "+err.Error())
			s.Logger.Warn("reconcile: waiting for VM termination", "instance_id", inst.ID, "error", err)
			return
		}
		if err := s.Hypervisor.RemoveInstanceFiles(inst); err != nil {
			inst.LastError = err.Error()
			_ = s.Store.UpdateInstance(ctx, inst, "ERROR", "reconciliation", "DECOMMISSION_CLEANUP_FAILED", "storage cleanup failed: "+err.Error())
			s.Logger.Error("reconcile: storage cleanup failed", "instance_id", inst.ID, "error", err)
			return
		}
		inst.PID = 0
		inst.LastError = ""
		inst.DecommissionedAt = time.Now().UTC()
		_ = s.transition(ctx, inst, model.StateDecommissioned, "reconciliation completed interrupted decommission")
		return
	}
	if inst.Status == model.StateHibernated {
		return
	}

	pid, qmpState, live, qmpErr := s.Hypervisor.Inspect(ctx, inst)
	if !live {
		switch inst.Status {
		case model.StateCreating:
			inst.LastError = "CPS restarted before provisioning completed and no matching QEMU process was found"
			_ = s.transition(ctx, inst, model.StateFailed, inst.LastError)
		case model.StateStarting:
			if inst.HibernateSnapshot != "" {
				exists, snapErr := s.Hypervisor.SnapshotExists(ctx, inst, inst.HibernateSnapshot)
				if snapErr == nil && exists {
					inst.PID = 0
					_ = s.transition(ctx, inst, model.StateHibernated, "recovered saved snapshot after interrupted start")
					return
				}
			}
			inst.LastError = "QEMU was not found during startup reconciliation"
			_ = s.transition(ctx, inst, model.StateFailed, inst.LastError)
		case model.StateStopping:
			inst.PID = 0
			inst.LastError = ""
			inst.StoppedAt = time.Now().UTC()
			_ = s.transition(ctx, inst, model.StateStopped, "QEMU exited after graceful shutdown request")
		case model.StateHibernating:
			exists, snapErr := s.Hypervisor.SnapshotExists(ctx, inst, inst.HibernateSnapshot)
			if snapErr == nil && exists {
				inst.PID = 0
				inst.LastError = ""
				_ = s.transition(ctx, inst, model.StateHibernated, "recovered hibernation snapshot after interrupted operation")
			} else {
				inst.LastError = "QEMU exited during hibernation but saved snapshot could not be verified"
				_ = s.transition(ctx, inst, model.StateFailed, inst.LastError)
			}
		case model.StateRunning, model.StatePaused, model.StateUnknown:
			inst.PID = 0
			inst.LastError = "QEMU process exited unexpectedly; inspect qemu.log and qemu-debug.log"
			_ = s.transition(ctx, inst, model.StateFailed, inst.LastError)
		case model.StateStopped, model.StateFailed:
			return
		}
		return
	}
	if pid != inst.PID {
		inst.PID = pid
		inst.UpdatedAt = time.Now().UTC()
	}
	if qmpState == "legacy-no-qmp" && qmpErr == nil {
		if candidate.PID != pid {
			inst.PID = pid
			inst.UpdatedAt = time.Now().UTC()
			_ = s.Store.UpdateInstance(ctx, inst, "WARN", "reconciliation", "LEGACY_VM_FOUND", "found pre-QMP VM by PID and root disk; only force_stop is available for lifecycle control")
		}
		s.Logger.Warn("legacy VM is alive but has no QMP socket; read-only inventory works, live lifecycle controls are limited", "instance_id", inst.ID, "pid", pid)
		return
	}
	if qmpErr != nil {
		if qmpState == "legacy-no-qmp" {
			s.Logger.Warn("legacy VM is alive but has no QMP socket; read-only inventory works, live lifecycle controls are limited", "instance_id", inst.ID, "pid", pid)
			if inst.PID != candidate.PID {
				_ = s.Store.UpdateInstance(ctx, inst, "WARN", "reconciliation", "LEGACY_VM_FOUND", "found pre-QMP VM by PID and root disk")
			}
			return
		}
		s.Logger.Warn("QEMU process exists but QMP inspection failed; retaining last known state", "instance_id", inst.ID, "pid", pid, "state", inst.Status, "error", qmpErr)
		if inst.PID != candidate.PID {
			_ = s.Store.UpdateInstance(ctx, inst, "WARN", "reconciliation", "PID_RECOVERED", "recovered QEMU PID from pidfile")
		}
		return
	}
	switch qmpState {
	case "running", "prelaunch":
		if inst.Status == model.StateHibernating {
			exists, _ := s.Hypervisor.SnapshotExists(ctx, inst, inst.HibernateSnapshot)
			if exists {
				if finishErr := s.Hypervisor.FinishHibernate(ctx, inst); finishErr != nil {
					s.Logger.Warn("cannot finish interrupted hibernation", "instance_id", inst.ID, "error", finishErr)
					return
				}
				inst.PID = 0
				_ = s.transition(ctx, inst, model.StateHibernated, "reconciliation completed interrupted hibernation")
				return
			}
			inst.HibernateSnapshot = ""
			_ = s.transition(ctx, inst, model.StateRunning, "hibernation did not produce a snapshot; VM remains running")
			return
		}
		if inst.Status == model.StateCreating || inst.Status == model.StateStarting || inst.Status == model.StateUnknown || inst.Status == model.StatePaused || inst.Status == model.StateStopped || inst.Status == model.StateFailed {
			inst.LastError = ""
			_ = s.transition(ctx, inst, model.StateRunning, "QMP query-status reports the VM process running; repaired stale inventory state")
		}
	case "paused", "suspended":
		// If CPS stopped while hibernation was in progress, a valid snapshot plus
		// a still-paused QEMU means we can finish the operation instead of leaving
		// the VM stranded in HIBERNATING forever.
		if inst.Status == model.StateHibernating {
			exists, snapshotErr := s.Hypervisor.SnapshotExists(ctx, inst, inst.HibernateSnapshot)
			if snapshotErr == nil && exists {
				if finishErr := s.Hypervisor.FinishHibernate(ctx, inst); finishErr != nil {
					s.Logger.Warn("cannot finish interrupted hibernation for paused VM", "instance_id", inst.ID, "error", finishErr)
					return
				}
				inst.PID = 0
				inst.LastError = ""
				_ = s.transition(ctx, inst, model.StateHibernated, "reconciliation completed interrupted hibernation from paused QEMU state")
				return
			}
			inst.HibernateSnapshot = ""
			_ = s.transition(ctx, inst, model.StatePaused, "hibernation snapshot was unavailable; keeping the guest paused")
			return
		}
		if inst.Status != model.StatePaused {
			_ = s.transition(ctx, inst, model.StatePaused, "QMP query-status reports paused guest execution")
		}
	case "shutdown":
		if inst.Status == model.StateStopping {
			// Give QEMU time to exit after the guest acknowledged the ACPI request.
			// If it does not exit, the next poll still shows STOPPING and force_stop remains available.
			return
		}
		inst.PID = 0
		inst.LastError = "QEMU status is shutdown outside a requested stop"
		_ = s.transition(ctx, inst, model.StateFailed, "QMP reports guest shutdown without an active STOPPING request")
	case "internal-error", "io-error":
		inst.PID = 0
		inst.LastError = "QEMU status is " + qmpState
		_ = s.transition(ctx, inst, model.StateFailed, "QMP reports terminal VM status: "+qmpState)
	default:
		s.Logger.Debug("unmapped QMP state retained for next reconciliation", "instance_id", inst.ID, "qmp_state", qmpState, "db_state", inst.Status)
	}
}

func (s *Service) hydrateLegacyPaths(ctx context.Context, candidate *model.Instance) (*model.Instance, error) {
	inst, err := s.Store.GetInstance(ctx, candidate.ID)
	if err != nil {
		return nil, err
	}
	changed := false
	defaultDir := filepath.Join(s.InstancesDir, inst.ID)
	if inst.InstanceDir == "" {
		inst.InstanceDir = defaultDir
		changed = true
	}
	if inst.RootDisk == "" {
		inst.RootDisk = filepath.Join(inst.InstanceDir, "root.qcow2")
		changed = true
	}
	if inst.SeedISO == "" {
		inst.SeedISO = filepath.Join(inst.InstanceDir, "cidata.iso")
		changed = true
	}
	if inst.PIDFile == "" {
		inst.PIDFile = filepath.Join(inst.InstanceDir, "qemu.pid")
		changed = true
	}
	if inst.LogFile == "" {
		inst.LogFile = filepath.Join(inst.InstanceDir, "qemu.log")
		changed = true
	}
	if inst.SerialLogFile == "" {
		inst.SerialLogFile = filepath.Join(inst.InstanceDir, "serial.log")
		changed = true
	}
	if inst.DebugLogFile == "" {
		inst.DebugLogFile = filepath.Join(inst.InstanceDir, "qemu-debug.log")
		changed = true
	}
	if inst.NodeName == "" {
		inst.NodeName = s.NodeName
		changed = true
	}
	if inst.NodeID == "" {
		inst.NodeID = s.NodeID
		changed = true
	}
	if inst.Region == "" {
		inst.Region = s.Region
		changed = true
	}
	if inst.Zone == "" {
		inst.Zone = s.Zone
		changed = true
	}
	// Do not assign a QMP socket to an existing process if its launch arguments
	// predate QMP; otherwise PID ownership verification would reject that process.
	if inst.QMPsocket == "" {
		sock := filepath.Join(inst.InstanceDir, "qmp.sock")
		if _, err := os.Stat(sock); err == nil {
			inst.QMPsocket = sock
			changed = true
		}
	}
	if inst.SSHHost == "" {
		inst.SSHHost = "127.0.0.1"
		changed = true
	}
	if inst.SSHUser == "" {
		inst.SSHUser = defaultSSHUser(inst.ImageName)
		changed = true
	}
	if changed {
		inst.UpdatedAt = time.Now().UTC()
		if err := s.Store.UpdateInstance(ctx, inst, "INFO", "reconciliation", "LEGACY_METADATA_RECOVERED", "filled missing metadata fields for existing CPS inventory record"); err != nil {
			return nil, err
		}
		s.Logger.Info("legacy instance metadata recovered", "instance_id", inst.ID)
	}
	return inst, nil
}

func (s *Service) StartReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.Reconcile(ctx)
			}
		}
	}()
}

func (s *Service) emit(ctx context.Context, id, level, component, event, message string) {
	if err := s.Store.RecordEvent(ctx, model.Event{InstanceID: id, Level: level, Component: component, Event: event, Message: message, CreatedAt: time.Now().UTC()}); err != nil {
		s.Logger.Error("persist audit event", "instance_id", id, "event", event, "error", err)
	}
	if level == "ERROR" {
		s.Logger.Error(message, "instance_id", id, "component", component, "event", event)
	} else {
		s.Logger.Info(message, "instance_id", id, "component", component, "event", event)
	}
}

func (s *Service) defaultPublicKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, relative := range []string{".ssh/id_ed25519.pub", ".ssh/id_rsa.pub", ".ssh/id_ecdsa.pub"} {
		data, err := os.ReadFile(filepath.Join(home, relative))
		if err == nil && strings.TrimSpace(string(data)) != "" {
			return strings.TrimSpace(string(data))
		}
	}
	return ""
}

func validatePublicKey(key string) error {
	if strings.ContainsAny(key, "\r\n") {
		return fmt.Errorf("ssh_public_key must contain exactly one public-key line")
	}
	fields := strings.Fields(key)
	if len(fields) < 2 {
		return fmt.Errorf("ssh_public_key is not a valid OpenSSH public key")
	}
	if !strings.HasPrefix(fields[0], "ssh-") && !strings.HasPrefix(fields[0], "ecdsa-") && !strings.HasPrefix(fields[0], "sk-") {
		return fmt.Errorf("ssh_public_key must use an OpenSSH public key format")
	}
	if _, err := base64.StdEncoding.DecodeString(fields[1]); err != nil {
		return fmt.Errorf("ssh_public_key has invalid base64 key data")
	}
	return nil
}

func defaultSSHUser(image string) string {
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

func generateID() (string, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate instance ID: %w", err)
	}
	return "i-" + hex.EncodeToString(b), nil
}

func (s *Service) allocatePort(ctx context.Context) (int, error) {
	instances, err := s.Store.ListInstances(ctx)
	if err != nil {
		return 0, err
	}
	used := make(map[int]bool)
	for _, inst := range instances {
		if inst.Status != model.StateDecommissioned && inst.SSHPort > 0 {
			used[inst.SSHPort] = true
		}
	}
	for port := 2222; port < 60000; port++ {
		if used[port] {
			continue
		}
		listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			continue
		}
		_ = listener.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free SSH forwarding ports found in range 2222-59999")
}

func readMemInfo() (uint64, uint64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var total, available uint64
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = value / 1024
		case "MemAvailable:":
			available = value / 1024
		}
	}
	return total, available
}

func diskInfo(path string) (uint64, uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	block := uint64(stat.Bsize)
	return stat.Blocks * block / (1024 * 1024 * 1024), stat.Bavail * block / (1024 * 1024 * 1024)
}
func minUint64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
