# PCDS — Pluggable Configuration Distribution Server

A lightweight Go server that runs on an instructor VM and distributes personalized environment variables to students at the start of a lab. No accounts, no passwords — students are identified by their VM's unique domain name and get a consistent set of credentials every time they run the setup command.

---

## Setup

Run this block once on the instructor VM. It installs dependencies, builds the binary, and starts the server.

```bash
# Install git if needed
sudo apt-get install -y git

# Clone the repo
git clone https://github.com/csfeeser/pcds-server.git ~/pcds-server
cd ~/pcds-server

# Install Go if needed
if ! command -v go &>/dev/null; then
  wget -q https://go.dev/dl/go1.22.4.linux-amd64.tar.gz
  sudo rm -rf /usr/local/go
  sudo tar -C /usr/local -xzf go1.22.4.linux-amd64.tar.gz
  echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
  source ~/.bashrc
fi

# Build the binary
make build

# Start the server in background (automatically injects your FQDN into the setup script)
nohup ./pcds-server > ~/pcds-server/pcds-server.log 2>&1 &
echo "Server started with PID $!"
```

The server runs on port **2225** and is viewable in **aux2**.

---

## Using the Dashboard

Open the dashboard in aux2 at port 2225. You'll see a setup form before any students connect.

### 1. Add static variables

These are values that are **the same on every student machine** — things like a shared tenant ID, subscription ID, or region. Click **+ Add Variable** for each one and fill in the name and value.

### 2. Paste your unique variables (CSV)

These are values that **differ per student**. Values in the same row are always assigned together.

Paste a CSV into the text area. The first row is headers (variable names); each subsequent row is one student's slot, assigned first-come first-served when a student runs the setup command.

```
ARM_CLIENT_ID,ARM_CLIENT_SECRET,ARM_RESOURCE_GROUP
aaa-111,s3cr3t1,rg-student-01
bbb-222,s3cr3t2,rg-student-02
```

<details>
<summary><strong>Teaching the Terraform on Azure course? Click here for instructions on how to get this data and format it.</strong></summary>

> **Placeholder** — instructions for acquiring Azure service principal credentials and resource group names, and formatting them as a CSV for this tool, will go here.

</details>

### 3. Open the session

Click **Save & Open for Students**. The page switches to the live roster view.

### 4. Share the setup command with students

The roster page displays the command students need to run. Copy it and share it with the class. Students run it once in their terminal, enter their name when prompted, and their environment is configured immediately and persists across future sessions.

---

## Resetting

To wipe all student assignments and clear the config (e.g. between class sessions), click the **✖ Reset** button in the roster view and confirm. The page returns to the setup form automatically.

To fully stop the server:

```bash
pkill pcds-server
```

---

## Student Experience

Students run one command:

```bash
source <(curl -s http://<INSTRUCTOR_FQDN>:2225/setup.sh)
```

They enter their name when prompted. Their shell is configured immediately and the variables persist in all future terminal sessions. If a student runs the command again (e.g. after a crash or reset), they receive the same credentials they were originally assigned.

---

## Ports

| Port | Purpose |
|---|---|
| 2225 | PCDS server — student config requests, instructor dashboard, and `setup.sh` distribution |
| 8080 | nginx — no longer needed for PCDS |

---

## API

| Method | Path | Description |
|---|---|---|
| `GET`  | `/setup.sh`   | Student setup script with instructor FQDN pre-injected |
| `POST` | `/get-config` | Student registration — returns `export` lines for the shell |
| `GET` | `/roster` | Plain-text roster with slot usage (curl-friendly) |
| `POST` | `/config` | Save instructor configuration |
| `POST` | `/reset` | Wipe config and all student assignments |
| `GET` | `/events` | Server-sent events stream (used by the dashboard) |
