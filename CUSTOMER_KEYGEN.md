# Generate the L1 keys

Run this on a Linux x86-64 machine that will retain the private keys.

```bash
mkdir l1-keygen
cd l1-keygen

curl -fLO https://github.com/ava-labs/avalanche-benchmark/releases/download/l1-public-handover-20260723/l1
curl -fL -o nodes.ini https://github.com/ava-labs/avalanche-benchmark/releases/download/l1-public-handover-20260723/nodes.ini.example
chmod +x l1
```

Edit `nodes.ini` with the machines and roles for the deployment, then run:

```bash
./l1 keygen
sha256sum deployment/public.json
```

Return exactly these two things to Ava Labs:

1. `deployment/public.json`
2. The SHA-256 printed by `sha256sum`

Keep the entire `deployment/` directory secure. It contains the private node,
manager, and Genesis-funds keys. Do not send any other file from it.

The command fails if `deployment/` already exists. Move the existing directory
aside before generating a completely new set of keys.
