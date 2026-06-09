## Go SDK

Client library for Go applications to interact with the fabric.

```go
import "github.com/compute-nmonit/sdk-go/fabric"

client := fabric.NewClient("localhost:9000")

// Distributed memory allocation
buf, err := client.AllocDistributed(1 * 1024 * 1024 * 1024) // 1GB
defer client.FreeDistributed(buf)

// GPU memory
gpuBuf, err := client.AllocGPU(8 * 1024 * 1024 * 1024) // 8GB VRAM
defer client.FreeGPU(gpuBuf)

// Submit a job
job, err := client.SubmitJob(fabric.JobSpec{
    Image:   "pytorch:latest",
    GPUs:    2,
    VRAM:    "24G",
    Command: []string{"python", "train.py"},
})
```
