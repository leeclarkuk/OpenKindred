import sys, glob, yaml, jsonschema
crds = {}
for f in glob.glob("api/v1alpha1/*.yaml"):
    d = yaml.safe_load(open(f))
    for v in d["spec"]["versions"]:
        crds[d["spec"]["names"]["kind"]] = v["schema"]["openAPIV3Schema"]
bad = 0
for f in glob.glob("examples/*.yaml"):
    for doc in yaml.safe_load_all(open(f)):
        s = crds[doc["kind"]]
        try: jsonschema.validate(doc, s); print("ok", doc["kind"], doc["metadata"]["name"])
        except jsonschema.ValidationError as e: bad = 1; print("FAIL", doc["kind"], e.message)
# negative: McpServer with both url and image must fail
try: jsonschema.validate({"spec":{"transport":"sse","url":"x","image":"y"}}, crds["McpServer"]); print("FAIL oneOf not enforced"); bad=1
except jsonschema.ValidationError: print("ok oneOf rejects both")
sys.exit(bad)
