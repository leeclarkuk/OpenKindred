#!/usr/bin/env python3
"""Render the Agent composition with the real Crossplane render engine and check the result.

Needs the crossplane CLI. Either docker (the default, pulls the function image), or the
no-docker path: set CROSSPLANE_BIN to a crossplane binary and FUNCTION_BIN to a
function-go-templating binary (see hack/build-render-tools.sh). Needs go, to validate rendered
configuration with the runtime's own parser, and PyYAML.

    CROSSPLANE_CLI=... CROSSPLANE_BIN=... FUNCTION_BIN=... python3 hack/test-composition.py
"""
import copy, json, os, pathlib, shutil, socket, subprocess, sys, tempfile, time

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent
AGENT = ROOT / "platform/crossplane/agent"
CLI = os.environ.get("CROSSPLANE_CLI", "crossplane")
BIN = os.environ.get("CROSSPLANE_BIN")
FUNC = os.environ.get("FUNCTION_BIN")

failures = []
checks = 0


def check(cond, msg):
    global checks
    checks += 1
    if not cond:
        failures.append(msg)
        print("  FAIL:", msg)


def docs(text):
    return [d for d in yaml.safe_load_all(text) if d]


def model(name="default", ns="demo", **spec):
    s = {"provider": "openai-compatible", "model": "example-model", "endpoint": "http://vllm.models.svc:8000/v1"}
    s.update(spec)
    return {"apiVersion": "openkindred.io/v1alpha1", "kind": "Model", "metadata": {"name": name, "namespace": ns}, "spec": s}


def mcp(name="docs-search", ns="demo", **spec):
    s = {"transport": "streamable-http", "url": "http://docs-search.tools.svc:8080/mcp"}
    s.update(spec)
    return {"apiVersion": "openkindred.io/v1alpha1", "kind": "McpServer", "metadata": {"name": name, "namespace": ns}, "spec": s}


def agent(tools=None, instructions="Answer using the docs-search tool.", model_name="default"):
    spec = {"modelRef": {"name": model_name}, "instructions": instructions}
    spec["tools"] = tools if tools is not None else [{"mcpServerRef": {"name": "docs-search"}}]
    return {"apiVersion": "openkindred.io/v1alpha1", "kind": "Agent", "metadata": {"name": "helper", "namespace": "demo"}, "spec": spec}


class Renderer:
    def __init__(self, tmp):
        self.tmp = pathlib.Path(tmp)
        self.n = 0
        if FUNC:
            self.functions = AGENT / "tests/functions.yaml"
        else:
            f = yaml.safe_load((AGENT / "tests/functions.yaml").read_text())
            f["metadata"].pop("annotations", None)
            self.functions = self.tmp / "functions.yaml"
            self.functions.write_text(yaml.safe_dump(f))

    def write(self, name, objs):
        p = self.tmp / f"{self.n}-{name}.yaml"
        p.write_text(yaml.safe_dump_all(objs))
        return str(p)

    def render(self, xr, resources, observed=None):
        self.n += 1
        cmd = [CLI, "render", self.write("xr", [xr]), str(AGENT / "composition.yaml"), str(self.functions),
               "--required-resources=" + self.write("res", resources), "-x"]
        if BIN:
            cmd.append("--crossplane-binary=" + BIN)
        if observed:
            cmd.append("--observed-resources=" + self.write("obs", observed))
        out = subprocess.run(cmd, capture_output=True, text=True)
        if out.returncode != 0:
            print(out.stdout, out.stderr)
            raise SystemExit("render failed")
        return Rendered(docs(out.stdout))


class Rendered:
    def __init__(self, ds):
        self.xr = next(d for d in ds if d["kind"] == "Agent")
        self.by_kind = {d["kind"]: d for d in ds if d["kind"] != "Agent"}

    def cond(self, t):
        for c in self.xr.get("status", {}).get("conditions", []):
            if c["type"] == t:
                return c
        return {}

    @property
    def cm(self):
        return self.by_kind["ConfigMap"]

    @property
    def config(self):
        return json.loads(self.cm["data"]["config.json"])

    @property
    def dep(self):
        return self.by_kind["Deployment"]

    @property
    def hash(self):
        return self.dep["spec"]["template"]["metadata"]["annotations"]["openkindred.io/template-hash"]


def runtime_check(config_json):
    p = subprocess.run(["go", "run", "./cmd/configcheck", "-"], cwd=ROOT / "runtime", input=config_json, capture_output=True, text=True)
    return p.returncode, (p.stdout + p.stderr).strip()


def observed_from(r, **status):
    """The Deployment as the cluster would report it, with the given status."""
    d = copy.deepcopy(r.dep)
    d["metadata"]["annotations"] = {"crossplane.io/composition-resource-name": "deployment"}
    d["metadata"]["generation"] = status.pop("generation", 3)
    base = {"observedGeneration": d["metadata"]["generation"], "replicas": 1, "updatedReplicas": 1, "availableReplicas": 1}
    base.update(status)
    d["status"] = base
    return d


def main():
    tmp = tempfile.mkdtemp()
    proc = None
    try:
        if FUNC:
            proc = subprocess.Popen([FUNC, "--insecure"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            for _ in range(100):
                try:
                    socket.create_connection(("localhost", 9443), timeout=0.2).close()
                    break
                except OSError:
                    time.sleep(0.1)
        R = Renderer(tmp)
        run(R)
    finally:
        if proc:
            proc.terminate()
        shutil.rmtree(tmp, ignore_errors=True)
    print(f"\n{checks} checks, {len(failures)} failed")
    sys.exit(1 if failures else 0)


def run(R):
    print("resolved agent")
    r = R.render(agent(), [model(), mcp(allowedTools=["search", "fetch"])])
    check(r.cond("ReferencesResolved").get("status") == "True", "references resolve")
    check(r.config["unresolved"] == [], "no unresolved references")
    rc, out = runtime_check(r.cm["data"]["config.json"])
    check(rc == 0, f"runtime accepts the rendered configuration: {out}")
    check(r.cm["metadata"]["name"] == f"helper-config-{r.xr['status']['configHash']}", "ConfigMap is named by its content hash")
    check(out.startswith(r.xr["status"]["configHash"]), "ConfigMap name suffix matches the hash the runtime reports")
    check(r.cm.get("immutable") is True, "ConfigMap is immutable")
    check(r.dep["spec"]["strategy"]["type"] == "Recreate", "Deployment uses Recreate")
    check(r.dep["spec"]["replicas"] == 1, "one replica")
    pod = r.dep["spec"]["template"]["spec"]
    c = pod["containers"][0]
    check(pod["securityContext"]["runAsNonRoot"] is True and c["securityContext"]["readOnlyRootFilesystem"] is True, "non-root, read-only root filesystem")
    check(c["securityContext"]["capabilities"]["drop"] == ["ALL"], "all capabilities dropped")
    check(pod["automountServiceAccountToken"] is False, "no service account token mounted")
    check(c["readinessProbe"]["httpGet"]["path"] == "/readyz" and c["livenessProbe"]["httpGet"]["path"] == "/healthz", "probes use /readyz and /healthz")
    check("env" not in c, "no credential environment when the Model has none")
    check(pod["volumes"][0]["configMap"]["name"] == r.cm["metadata"]["name"], "pod mounts the content-hashed ConfigMap")
    check(set(r.by_kind) >= {"ConfigMap", "Deployment", "Service", "ServiceAccount", "NetworkPolicy"}, "all five resources are composed")
    check(r.config["mcpServers"][0]["allowedTools"] == ["search", "fetch"] and "agentAllowedTools" not in r.config["mcpServers"][0], "server allow list carried, agent list omitted")
    check(r.cond("Ready").get("status") == "False", "not Ready before the Deployment has rolled out")

    print("determinism and change propagation")
    r2 = R.render(agent(), [model(), mcp(allowedTools=["search", "fetch"])])
    check(r2.cm["metadata"]["name"] == r.cm["metadata"]["name"] and r2.hash == r.hash, "same inputs give the same names and hash")
    r3 = R.render(agent(instructions="Different."), [model(), mcp(allowedTools=["search", "fetch"])])
    check(r3.cm["metadata"]["name"] != r.cm["metadata"]["name"] and r3.hash != r.hash, "changed instructions give a new ConfigMap and a new pod template")
    r4 = R.render(agent(), [model(endpoint="http://other.models.svc/v1"), mcp(allowedTools=["search", "fetch"])])
    check(r4.cm["metadata"]["name"] != r.cm["metadata"]["name"], "a changed Model endpoint reaches the configuration")
    r5 = R.render(agent(), [model(), mcp(url="http://moved.tools.svc/mcp", allowedTools=["search", "fetch"])])
    check(r5.config["mcpServers"][0]["url"] == "http://moved.tools.svc/mcp" and r5.hash != r.hash, "a changed McpServer URL reaches the configuration and the pod template")

    print("fail closed on missing references")
    m = R.render(agent(), [mcp()])
    check(m.cond("ReferencesResolved").get("status") == "False", "missing Model: ReferencesResolved is False")
    check("Model" in m.cond("ReferencesResolved").get("message", ""), "the condition names the missing Model")
    check({"Deployment", "Service", "ConfigMap"} <= set(m.by_kind), "resources are still rendered, so nothing is deleted")
    rc, out = runtime_check(m.cm["data"]["config.json"])
    check(rc != 0 and "Model" in out and "default" in out, f"runtime refuses the configuration with the reason: {out}")
    check(m.cond("Ready").get("status") == "False", "never Ready with an unresolved reference")
    ok = R.render(agent(), [model(), mcp()])
    obs = [observed_from(ok)]
    m2 = R.render(agent(), [mcp()], observed=[observed_from(m)])
    check(m2.cond("Ready").get("status") == "False", "even a fully available Deployment is not Ready while references are unresolved")
    mm = R.render(agent(), [model()])
    check("McpServer" in mm.cond("ReferencesResolved").get("message", ""), "missing McpServer is named")
    img = R.render(agent(), [model(), {"apiVersion": "openkindred.io/v1alpha1", "kind": "McpServer", "metadata": {"name": "docs-search", "namespace": "demo"}, "spec": {"transport": "sse", "image": "example/mcp:1"}}])
    check(img.cond("ReferencesResolved").get("status") == "False" and "image" in img.cond("ReferencesResolved").get("message", ""), "image based McpServer is rejected explicitly")
    other = R.render(agent(), [model(ns="other"), mcp()])
    check(other.cond("ReferencesResolved").get("status") == "False", "a Model in another namespace is not used")

    print("allow lists keep omitted and empty distinct")
    both = R.render(agent(tools=[{"mcpServerRef": {"name": "a"}, "allowedTools": []}, {"mcpServerRef": {"name": "b"}}]),
                    [model(), mcp("a", allowedTools=[]), mcp("b")])
    s = {x["name"]: x for x in both.config["mcpServers"]}
    check(s["a"].get("allowedTools") == [] and s["a"].get("agentAllowedTools") == [], "empty lists stay empty lists")
    check("allowedTools" not in s["b"] and "agentAllowedTools" not in s["b"], "omitted lists stay omitted")
    rc, out = runtime_check(both.cm["data"]["config.json"])
    check(rc == 0, f"runtime accepts it: {out}")

    print("credentials")
    cred = R.render(agent(), [model(credentialsRef={"name": "model-key"}), mcp()])
    c = cred.dep["spec"]["template"]["spec"]["containers"][0]
    ref = c["env"][0]["valueFrom"]["secretKeyRef"]
    check(ref == {"name": "model-key", "key": "api-key"}, "credential comes from a Secret reference with a default key")
    check(cred.config["model"]["apiKeyEnv"] == "OPENKINDRED_MODEL_API_KEY" and "model-key" not in cred.cm["data"]["config.json"], "configuration names the variable and never the Secret")
    rot = R.render(agent(), [model(credentialsRef={"name": "model-key-2"}), mcp()])
    check(rot.cm["metadata"]["name"] == cred.cm["metadata"]["name"], "rotating to a new Secret leaves the configuration unchanged")
    check(rot.hash != cred.hash, "but changes the pod template, so the Agent rolls")
    custom = R.render(agent(), [model(credentialsRef={"name": "k", "key": "token"}), mcp()])
    check(custom.dep["spec"]["template"]["spec"]["containers"][0]["env"][0]["valueFrom"]["secretKeyRef"]["key"] == "token", "a custom Secret key is honoured")

    print("rollout readiness")
    base = R.render(agent(), [model(), mcp()])
    done = R.render(agent(), [model(), mcp()], observed=[observed_from(base)])
    check(done.cond("WorkloadAvailable").get("status") == "True" and done.cond("Ready").get("status") == "True", "complete rollout of the current template is Ready")
    stale = observed_from(base)
    stale["spec"]["template"]["metadata"]["annotations"]["openkindred.io/template-hash"] = "000000000000"
    st = R.render(agent(), [model(), mcp()], observed=[stale])
    check(st.cond("Ready").get("status") == "False" and st.cond("WorkloadAvailable").get("status") == "False", "an available replica of an OLD template does not make the Agent Ready")
    lag = R.render(agent(), [model(), mcp()], observed=[observed_from(base, observedGeneration=2)])
    check(lag.cond("Ready").get("status") == "False", "a Deployment that has not observed its latest generation is not Ready")
    for label, extra in {"no updated replicas": {"updatedReplicas": 0}, "no available replicas": {"availableReplicas": 0},
                         "an unavailable replica": {"unavailableReplicas": 1}, "an extra old replica": {"replicas": 2}}.items():
        x = R.render(agent(), [model(), mcp()], observed=[observed_from(base, **extra)])
        check(x.cond("Ready").get("status") == "False", f"{label}: not Ready")
    changed = R.render(agent(instructions="Changed."), [model(), mcp()], observed=[observed_from(base)])
    check(changed.cond("Ready").get("status") == "False", "right after a config change, the previously ready Deployment is not Ready until it has rolled")


if __name__ == "__main__":
    main()
