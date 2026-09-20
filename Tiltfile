# e2ai dev stack. Mirrors incidents-admin: namespace-per-project, postgres from
# local/infra.yaml (throwaway local DB — staging uses shared RDS), the prebuilt
# sha-pinned admin-bff shell, and the env-to-secret bridge so pods see the same
# resolved environment a bare process would.
ENV = os.getenv("ENV", "")
STAGING = ENV == "staging"
ROOT = os.getcwd()
NS = "e2ai"
REG = "492684252576.dkr.ecr.us-east-1.amazonaws.com"
BFF_IMAGE = REG + "/admin-bff-http@sha256:1d62226bc40054ef868d96c3aa297e280d14a0b41f982097752fb949b14ee4b7"

allow_k8s_contexts(["orbstack"])

def dedupe_env(manifest_blob):
    out = []
    for obj in decode_yaml_stream(manifest_blob):
        if obj and obj.get("kind") == "Deployment":
            spec = obj.get("spec", {}).get("template", {}).get("spec", {})
            for container in spec.get("containers", []):
                envs = container.get("env")
                if envs:
                    last = {}
                    for index, environment in enumerate(envs):
                        last[environment.get("name")] = index
                    container["env"] = [environment for index, environment in enumerate(envs) if last[environment.get("name")] == index]
        out.append(str(encode_yaml(obj)) if obj else "---")
    return blob("\n---\n".join(out))

# The env secret owns the 1Password-injected vars (GITHUB_TOKEN, WEBHOOK_SECRET)
# plus any per-user overlays. Wave 1 wires api/runner images + the helm chart
# onto this same secret.
k8s_yaml(local(
    ["bash", "local/env-to-secret.sh"],
    env={"ENV": ENV, "NAMESPACE": NS, "SECRET_NAME": "e2ai-env"},
    quiet=True,
))
bff_cfg = "config/admin-bff.staging.yaml" if STAGING else "config/admin-bff.yaml"
k8s_yaml(local(
    ("kubectl create configmap admin-bff-config -n %s " +
     "--from-file=config.yaml=%s --from-file=local.yaml=%s " +
     "--dry-run=client -o yaml") % (NS, bff_cfg, bff_cfg),
    quiet=True,
))
local_resource(
    "admin-bff-image",
    cmd="docker image inspect " + BFF_IMAGE + " >/dev/null 2>&1 || " +
        "(aws ecr get-login-password --region us-east-1 | docker login --username AWS --password-stdin " +
        REG + " && docker pull --platform linux/amd64 " + BFF_IMAGE + ")",
)
k8s_yaml(dedupe_env(local_yaml("local/infra.yaml")))

# Fixed service forwards (ports 8082 api / 5434 postgres / 3005 web-module
# forward arrives with the Wave 2 web resource; the runner has no port).
k8s_resource("postgres", port_forwards=["5434:5432"])
k8s_resource("admin-bff", port_forwards=["8080"], resource_deps=["admin-bff-image"])
# Wave 1: docker_build(REG + "/e2ai-api", ...) + helm + k8s_resource("api", ["8082"]).

# build marker
