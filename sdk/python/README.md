## Python SDK

### Usage (Planned API)

```python
import compute_fabric as cf

# Connect to cluster
client = cf.Client("localhost:8080")

# Distributed numpy array
arr = cf.array((10000, 10000), dtype="float32")
# ^ Strips across nodes transparently

# GPU array in pooled VRAM
gpu_arr = cf.gpu.array((4096, 4096), dtype="float16")

# Submit a job
job = cf.submit(
    image="pytorch:latest",
    gpus=2,
    vram="24G",
    command=["python", "train.py"],
)

# Check cluster status
print(cf.status())
```
