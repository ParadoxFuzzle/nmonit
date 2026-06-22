// Package validator provides input validation for gRPC requests.
//
// All validators return a gRPC status error with codes.InvalidArgument
// when validation fails, or nil when the input is valid.
package validator

import (
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Bounds for string identifiers and numeric fields.
const (
	MaxNodeIDLen         = 255
	MaxTaskIDLen         = 255
	MaxJobIDLen          = 255
	MaxAllocationIDLen   = 255
	MaxContainerImageLen = 1024
	MaxCommandLen        = 256 * 1024 // 256KB total command+args
	MaxEnvKeyLen         = 128
	MaxEnvValueLen       = 4096
	MaxEnvCount          = 128
	MaxMemoryBytes       = 1 << 40   // 1 TB
	MaxGPUMemBytes       = 256 << 30 // 256 GB
	MaxReplicas          = 100
	MaxPriority          = 100
	MaxTasksPerHeartbeat = 10000
)

// SafeIDPattern matches strings consisting of alphanumerics, hyphens,
// underscores, and dots. Colons are not allowed (reserved for image refs).
var SafeIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9\-_.]*$`)

// ContainerImagePattern loosely validates an OCI container image reference.
// Allows: [registry[:port]/]name[:tag] or [registry[:port]/]name[@sha256:...]
var ContainerImagePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.\-_]*(:[0-9]+)?(/[a-zA-Z0-9][a-zA-Z0-9.\-_]*)*(:(?i)[a-zA-Z0-9.\-_]+)?(@sha256:[a-fA-F0-9]{64})?$`)

// ValidateStringID checks that s is non-empty, within maxLen, has no null
// bytes, and matches the safe ID character set. The fieldName is used in
// error messages.
func ValidateStringID(s, fieldName string, maxLen int) error {
	if s == "" {
		return status.Errorf(codes.InvalidArgument, "%s must not be empty", fieldName)
	}
	if HasNullBytes(s) {
		return status.Errorf(codes.InvalidArgument, "%s contains null bytes", fieldName)
	}
	if len(s) > maxLen {
		return status.Errorf(codes.InvalidArgument, "%s exceeds max length %d (got %d)", fieldName, maxLen, len(s))
	}
	if !SafeIDPattern.MatchString(s) {
		return status.Errorf(codes.InvalidArgument, "%s contains invalid characters", fieldName)
	}
	return nil
}

// ValidateNodeID validates a node identifier.
func ValidateNodeID(nodeID string) error {
	return ValidateStringID(nodeID, "node_id", MaxNodeIDLen)
}

// ValidateTaskID validates a task identifier.
func ValidateTaskID(taskID string) error {
	return ValidateStringID(taskID, "task_id", MaxTaskIDLen)
}

// ValidateJobID validates a job identifier.
func ValidateJobID(jobID string) error {
	return ValidateStringID(jobID, "job_id", MaxJobIDLen)
}

// ValidateAllocationID validates an allocation identifier.
func ValidateAllocationID(allocID string) error {
	return ValidateStringID(allocID, "allocation_id", MaxAllocationIDLen)
}

// ValidateContainerImage validates an OCI container image reference.
func ValidateContainerImage(image string) error {
	if image == "" {
		return status.Error(codes.InvalidArgument, "container_image must not be empty")
	}
	if HasNullBytes(image) {
		return status.Error(codes.InvalidArgument, "container_image contains null bytes")
	}
	if len(image) > MaxContainerImageLen {
		return status.Errorf(codes.InvalidArgument, "container_image exceeds max length %d", MaxContainerImageLen)
	}
	if !ContainerImagePattern.MatchString(image) {
		return status.Errorf(codes.InvalidArgument, "container_image has invalid format")
	}
	return nil
}

// ValidateMemorySize checks that size is within acceptable bounds.
func ValidateMemorySize(size uint64) error {
	if size == 0 {
		return status.Error(codes.InvalidArgument, "size_bytes must be > 0")
	}
	if size > MaxMemoryBytes {
		return status.Errorf(codes.InvalidArgument, "size_bytes exceeds max (%d bytes)", MaxMemoryBytes)
	}
	return nil
}

// ValidateGPUMemorySize checks that GPU memory size is within acceptable bounds.
func ValidateGPUMemorySize(size uint64) error {
	if size == 0 {
		return status.Error(codes.InvalidArgument, "size_bytes must be > 0")
	}
	if size > MaxGPUMemBytes {
		return status.Errorf(codes.InvalidArgument, "size_bytes exceeds max (%d bytes)", MaxGPUMemBytes)
	}
	return nil
}

// ValidateReplicationFactor checks the replication_factor range.
func ValidateReplicationFactor(n int32) error {
	if n < 0 || n > MaxReplicas {
		return status.Errorf(codes.InvalidArgument, "replication_factor must be 0-%d", MaxReplicas)
	}
	return nil
}

// ValidatePriority checks that priority is within 0-MaxPriority.
func ValidatePriority(p int32) error {
	if p < 0 || p > MaxPriority {
		return status.Errorf(codes.InvalidArgument, "priority must be 0-%d", MaxPriority)
	}
	return nil
}

// ValidateEnvVars checks environment variable count and key/value lengths.
func ValidateEnvVars(env map[string]string) error {
	if len(env) > MaxEnvCount {
		return status.Errorf(codes.InvalidArgument, "env exceeds max %d entries", MaxEnvCount)
	}
	for k, v := range env {
		if len(k) == 0 || len(k) > MaxEnvKeyLen {
			return status.Errorf(codes.InvalidArgument, "env key length must be 1-%d", MaxEnvKeyLen)
		}
		if len(v) > MaxEnvValueLen {
			return status.Errorf(codes.InvalidArgument, "env value exceeds max %d bytes", MaxEnvValueLen)
		}
	}
	return nil
}

// ValidateCommands checks that the command+args total doesn't exceed the limit.
func ValidateCommands(cmds []string) error {
	total := 0
	for _, c := range cmds {
		total += len(c)
	}
	if total > MaxCommandLen {
		return status.Errorf(codes.InvalidArgument, "command+args total exceeds max %d bytes", MaxCommandLen)
	}
	return nil
}

// ValidateHeartbeatTaskCount checks that the number of tasks in a heartbeat
// doesn't exceed a reasonable limit.
func ValidateHeartbeatTaskCount(n int) error {
	if n > MaxTasksPerHeartbeat {
		return status.Errorf(codes.InvalidArgument, "tasks count exceeds max %d", MaxTasksPerHeartbeat)
	}
	return nil
}

// ValidateGPUDeviceIndex checks the gpu_device_index range (-1 = any, 0+ = specific).
func ValidateGPUDeviceIndex(idx int32) error {
	if idx < -1 {
		return status.Error(codes.InvalidArgument, "gpu_device_index must be >= -1")
	}
	return nil
}

// ValidateVersion checks the agent version string length.
func ValidateVersion(v string) error {
	if HasNullBytes(v) {
		return status.Error(codes.InvalidArgument, "agent_version contains null bytes")
	}
	if len(v) > 64 {
		return status.Errorf(codes.InvalidArgument, "agent_version exceeds max length 64")
	}
	return nil
}

// ValidateAddress checks the address string length.
func ValidateAddress(addr string) error {
	if HasNullBytes(addr) {
		return status.Error(codes.InvalidArgument, "address contains null bytes")
	}
	if len(addr) > 512 {
		return status.Errorf(codes.InvalidArgument, "address exceeds max length 512")
	}
	return nil
}

// HasNullBytes checks if a string contains null bytes (common attack vector).
func HasNullBytes(s string) bool {
	return strings.ContainsRune(s, 0)
}
