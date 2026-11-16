# PCDS — Student Credential Handout

A single Python script (standard library only, Python 3.8+) that serves the Azure credentials created by
[`provision_students.sh`](https://static.alta3.com/courses/terraform-azure/resources/provision_students.sh)
as a page with one button per student. Clicking a button copies a shell block to your clipboard and grays the
button out, so you can see which ones you've already handed out. Paste the block into that student's terminal.

## Run it

```bash
git clone https://github.com/csfeeser/pcds-server.git ~/pcds-server
cd ~/pcds-server
# put student_credentials.csv from provision_students.sh here (optional — you can also upload it in the page)
nohup ./pcds.py > pcds.log 2>&1 &
```

Open `http://<instructor-host>:8092` and log in as **instructor** / **alta3**. The server listens on all
interfaces (`0.0.0.0:8092`).

Options: `--csv PATH` (default `student_credentials.csv`), `--state PATH` (default `pcds_state.json`),
`--port N` (default 8092).

## How it behaves

- **Loading credentials.** At startup the server loads `student_credentials.csv` if it's there. You can also
  load a CSV with **Upload CSV** or **Paste CSV** on the page. Loading a different CSV clears the used marks.
- **Used marks** are saved in `pcds_state.json`, so they survive page refreshes and server restarts, and they
  sync between open tabs. To un-gray one button, click its **unmark** link. To un-gray every button, use
  **Clear all marks**. Clicking a grayed button copies its block again.
- **What gets copied.** You get every CSV column as an `export`, plus `STUDENT_NUMBER`, which is taken from the
  resource group name (`rg-student-07` → `07`). Pasting the block on a student machine:
  1. removes any earlier PCDS block from `~/.bashrc`,
  2. appends the new `export` lines between `# >>> PCDS >>>` / `# <<< PCDS <<<` markers,
  3. loads the variables into the current shell.

  Every new terminal then has the variables. Pasting a different student's block replaces the old one.

`student_credentials.csv` and `pcds_state.json` contain service-principal secrets. Both are git-ignored, and
the state file is written with mode 600.
