package model

import "time"

type State string

const (
	StateCreating        State = "CREATING"
	StateStarting        State = "STARTING"
	StateRunning         State = "RUNNING"
	StateStopping        State = "STOPPING"
	StateStopped         State = "STOPPED"
	StatePaused          State = "PAUSED"
	StateHibernating     State = "HIBERNATING"
	StateHibernated      State = "HIBERNATED"
	StateDecommissioning State = "DECOMMISSIONING"
	StateDecommissioned  State = "DECOMMISSIONED"
	StateFailed          State = "FAILED"
	StateUnknown         State = "UNKNOWN"
)

type LaunchRequest struct {
	ImageName    string `json:"image_name,omitempty"`
	VCPUs        int    `json:"vcpus,omitempty"`
	MemoryMB     int    `json:"memory_mb,omitempty"`
	DiskSizeGB   int    `json:"disk_size_gb,omitempty"`
	SSHPublicKey string `json:"ssh_public_key,omitempty"`
	SSHUser      string `json:"ssh_user,omitempty"`
	UserData     string `json:"user_data,omitempty"`
}

type ActionRequest struct {
	Action string `json:"action"`
}

type UpdateInstanceRequest struct {
	VCPUs      *int `json:"vcpus,omitempty"`
	MemoryMB   *int `json:"memory_mb,omitempty"`
	DiskSizeGB *int `json:"disk_size_gb,omitempty"`
}

type Instance struct {
	ID                string    `json:"id"`
	PID               int       `json:"pid"`
	Status            State     `json:"status"`
	SSHHost           string    `json:"ssh_host"`
	SSHPort           int       `json:"ssh_port"`
	SSHUser           string    `json:"ssh_user"`
	VCPUs             int       `json:"vcpus"`
	MemoryMB          int       `json:"memory_mb"`
	DiskSizeGB        int       `json:"disk_size_gb"`
	ImageName         string    `json:"image_name"`
	RootDisk          string    `json:"root_disk"`
	SeedISO           string    `json:"seed_iso"`
	InstanceDir       string    `json:"instance_dir"`
	QMPsocket         string    `json:"qmp_socket"`
	PIDFile           string    `json:"pid_file"`
	LogFile           string    `json:"log_file"`
	SerialLogFile     string    `json:"serial_log_file"`
	DebugLogFile      string    `json:"debug_log_file"`
	HibernateSnapshot string    `json:"hibernate_snapshot,omitempty"`
	NodeID            string    `json:"node_id"`
	NodeName          string    `json:"node_name"`
	Region            string    `json:"region"`
	Zone              string    `json:"zone"`
	LastError         string    `json:"last_error,omitempty"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	StartedAt         time.Time `json:"started_at,omitempty"`
	StoppedAt         time.Time `json:"stopped_at,omitempty"`
	DecommissionedAt  time.Time `json:"decommissioned_at,omitempty"`
}

type Image struct {
	Name          string   `json:"name"`
	Aliases       []string `json:"aliases,omitempty"`
	Path          string   `json:"path"`
	Format        string   `json:"format,omitempty"`
	VirtualSizeGB float64  `json:"virtual_size_gb,omitempty"`
	ActualSizeGB  float64  `json:"actual_size_gb,omitempty"`
	Readable      bool     `json:"readable"`
	Error         string   `json:"error,omitempty"`
}

type ClusterMetadata struct {
	NodeID                string         `json:"node_id"`
	NodeName              string         `json:"node_name"`
	Region                string         `json:"region"`
	Zone                  string         `json:"zone"`
	OS                    string         `json:"os"`
	Architecture          string         `json:"architecture"`
	HostCPUCount          int            `json:"host_cpu_count"`
	HostMemTotalMB        uint64         `json:"host_mem_total_mb"`
	HostMemAvailableMB    uint64         `json:"host_mem_available_mb"`
	HostDiskTotalGB       uint64         `json:"host_disk_total_gb"`
	HostDiskAvailableGB   uint64         `json:"host_disk_available_gb"`
	AllocatedDiskGB       uint64         `json:"allocated_disk_gb"`
	AllocatableDiskGB     uint64         `json:"allocatable_disk_gb"`
	AllocatedVCPUs        int            `json:"allocated_vcpus"`
	AllocatableVCPUs      int            `json:"allocatable_vcpus"`
	AllocatedMemoryMB     int            `json:"allocated_memory_mb"`
	AllocatableMemoryMB   uint64         `json:"allocatable_memory_mb"`
	SupportedImages       []string       `json:"supported_images"`
	Images                []Image        `json:"images"`
	InstanceCountsByState map[string]int `json:"instance_counts_by_state"`
}

type Event struct {
	ID         int64     `json:"id"`
	InstanceID string    `json:"instance_id,omitempty"`
	Level      string    `json:"level"`
	Component  string    `json:"component"`
	Event      string    `json:"event"`
	Message    string    `json:"message"`
	CreatedAt  time.Time `json:"created_at"`
}

type Inventory struct {
	Metadata  ClusterMetadata `json:"metadata"`
	Instances []Instance      `json:"instances"`
	Events    []Event         `json:"recent_events,omitempty"`
}
