# PCDS — Pluggable Configuration Distribution Server

A lightweight Go server that runs on an instructor's VM and distributes personalized environment variables to students at the start of a lab. No accounts, no passwords, no credit cards — students are identified by their VM's unique domain name.

## How It Works

1. The instructor starts the PCDS server on their VM and writes a `generate_env.sh` script that provisions whatever the course needs (Azure service principals, API keys, etc.)
2. Students run a one-liner in their terminal
3. They enter their name when prompted
4. The server runs `generate_env.sh` once for that student, saves the output, and sends it back
5. The student's shell is configured immediately and permanently — env vars survive new terminal sessions

If a student runs the setup again (lost their env, crashed, etc.) they get the exact same output back. The script is never run twice for the same student, which prevents duplicate Azure resources, duplicate API key allocations, etc.

---

## Files

| File | Purpose |
|---|---|
| `main.go` | Go server source |
| `go.mod` | Go module file |
| `Makefile` | Build instructions |
| `generate_env.sh` | Payload generator — **replace this with your real provisioning logic** |
| `client_setup.sh` | Student-side setup script — distribute via nginx or SSH |

The server also produces these at runtime:

| File | Purpose |
|---|---|
| `pcds-server` | Compiled binary (produced by `make build`) |
| `students.json` | Auto-generated roster + cached payloads |

---

## Instructor Setup

### 1. Install Go

```bash
wget https://go.dev/dl/go1.22.4.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.22.4.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
go version
```

### 2. Build the Binary

```bash
make build
```

Produces `pcds-server` in the current directory.

### 3. Write Your `generate_env.sh`

The mock version prints three placeholder variables. Replace it with real logic for your course.

**Contract the script must follow:**
- `$1` = student name (e.g. `"Alice Smith"`)
- `$2` = student UUID domain (e.g. `"bchd.c2af2850-f31d-42f3-9b40-37b32f1824b5"`)
- Print `export KEY="VALUE"` lines to **stdout** — these become the student's env vars
- Send any progress or error messages to **stderr**
- Exit non-zero to signal failure (server returns HTTP 500 to the student)

Example for Azure (instructor must be logged in via `az login` before starting the server):

```bash
#!/bin/bash
STUDENT_NAME="$1"
STUDENT_UUID="$2"

CLEAN_ID=$(echo "$STUDENT_UUID" | tr -cd '[:alnum:]' | cut -c1-20)
RG_NAME="rg-class-$CLEAN_ID"
SP_NAME="sp-class-$CLEAN_ID"

az group create --name "$RG_NAME" --location "eastus" > /dev/null

SP_JSON=$(az ad sp create-for-rbac --name "$SP_NAME" \
    --role "Contributor" \
    --scopes "/subscriptions/$(az account show --query id -o tsv)/resourceGroups/$RG_NAME" \
    --sdk-auth 2>/dev/null)

CLIENT_ID=$(echo "$SP_JSON"     | grep -oP '"clientId": "\K[^"]+')
CLIENT_SECRET=$(echo "$SP_JSON" | grep -oP '"clientSecret": "\K[^"]+')
TENANT_ID=$(echo "$SP_JSON"     | grep -oP '"tenantId": "\K[^"]+')

echo "export ARM_CLIENT_ID=\"${CLIENT_ID}\""
echo "export ARM_CLIENT_SECRET=\"${CLIENT_SECRET}\""
echo "export ARM_TENANT_ID=\"${TENANT_ID}\""
echo "export ARM_SUBSCRIPTION_ID=\"$(az account show --query id -o tsv)\""
echo "export TF_VAR_resource_group=\"${RG_NAME}\""
```

### 4. Bake Your FQDN into the Client Script

```bash
sed -i "s/REPLACE_WITH_INSTRUCTOR_FQDN/$(hostname --fqdn)/" client_setup.sh
```

### 5. Publish the Client Script

Copy it to nginx's static directory so students can fetch it over HTTP:

```bash
sudo cp client_setup.sh /var/www/static/setup.sh
```

### 6. Start the Server

```bash
./pcds-server
```

Leave this terminal open. Every student request prints a log line — `CACHE MISS` on first contact, `CACHE HIT` on repeats.

---

## Verify It Works

From a second terminal on the instructor VM:

```bash
# Test the endpoint
curl -s -X POST http://localhost:2225/get-config \
  -H "Content-Type: application/json" \
  -d '{"name": "Test Student", "uuid_domain": "bchd.test-uuid-1234"}'

# View the live roster
curl http://localhost:2225/roster

# Inspect the state file
cat students.json
```

---

## Student Instructions

The instructor shares their FQDN at the start of class. Each student runs this once:

```bash
source <(curl -s http://bchd.INSTRUCTOR_FQDN:8080/setup.sh)
```

They enter their name when prompted. Done. Verify it worked:

```bash
env | grep PCDS       # mock vars
env | grep ARM        # Azure vars (real generate_env.sh)
```

---

## Ports

| Port | What |
|---|---|
| 2225 | PCDS server — student config requests |
| 8080 | nginx static server — distributes `setup.sh` to students |

---

## API

### `POST /get-config`

Request body (JSON):
```json
{
  "name": "Alice Smith",
  "uuid_domain": "bchd.c2af2850-f31d-42f3-9b40-37b32f1824b5"
}
```

Response: `200 text/plain` — the raw stdout of `generate_env.sh`, identical on every call for the same UUID domain.

Errors: `400` if fields are missing, `500` if the script fails.

### `GET /roster`

Returns a plain-text table of all registered students, their UUID domains, and registration timestamps.
