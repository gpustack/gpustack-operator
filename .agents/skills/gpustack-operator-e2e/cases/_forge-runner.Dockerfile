# Lightweight reference ForgeSandbox External runner image for case-38.sh Leg B.
#
# Build from the repository root:
#   docker build \
#     -f .agents/skills/gpustack-operator-e2e/cases/_forge-runner.Dockerfile \
#     -t <namespace>/gpustack-forge-runner-test:<tag> \
#     .agents/skills/gpustack-operator-e2e/cases
#
# Emits fused Triton kernels (rmsnorm, activation_quant, layernorm, rope_kv_append)
# that are independently compiled, benchmarked, and numerically verified on the GPU
# inside the engine-image forge-verifier initContainer.
FROM python:3.12-slim

COPY --chmod=0755 _forge-runner.py /usr/local/bin/forge-runner.py

USER 65532:65532

ENTRYPOINT ["python3", "/usr/local/bin/forge-runner.py"]
