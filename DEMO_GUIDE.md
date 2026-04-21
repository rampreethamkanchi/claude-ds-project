# Distributed Collaborative Editor: Demo Guide

This guide contains everything your team needs to set up the 3-laptop demo for the professor.

## 1. Preparation (On Your Laptop)

Since only one laptop has Go installed, you will act as the "Release Manager."

1. **Build the Binary:**
   Open a terminal and run:
   ```powershell
   go build -o server.exe ./cmd/server
   ```
2. **Push to GitHub:**
   Commit and push the `server.exe`, `web/` folder, and `config.json.template` to your repo so your teammates can pull them.

## 2. Setup (On Every Laptop)

Every team member must do the following:

### A. Get the code
```powershell
git pull
```

### B. Find your IP Address
Open PowerShell and run:
```powershell
ipconfig
```
Look for **IPv4 Address** under your Wi-Fi adapter (e.g., `192.168.1.15`). **Write this down.**

### C. Unblock the Firewall (IMPORTANT)
Choose **ONE** of these methods:

**Method 1: PowerShell (Fastest)**
- Run PowerShell as **Administrator** and paste:
  ```powershell
  New-NetFirewallRule -DisplayName "DistributedEditor" -Direction Inbound -Action Allow -Protocol TCP -LocalPort 12000,8080
  ```

**Method 2: Windows GUI**
1. Search for "Windows Defender Firewall with Advanced Security" in the Start Menu.
2. Click **Inbound Rules** (left) -> **New Rule...** (right).
3. Port -> Next -> TCP -> Enter `12000, 8080` -> Next.
4. Allow the connection -> Next -> Check all (Domain, Private, Public) -> Next.
5. Name it `DistributedEditor` -> Finish.

---

## 3. Configuration

1. Copy `config.json.template` to a new file named `config.json`.
2. Edit `config.json` and update the following:
   - `node_id`: Set to `node-1`, `node-2`, or `node-3`.
   - `grpc_addr`: Set to `YOUR_IP:12000`.
   - `ws_addr`: Set to `YOUR_IP:8080`.
   - `peers`: List the `grpc_addr` of all 3 laptops.

**Example `config.json` for Node 1:**
```json
{
    "node_id": "node-1",
    "grpc_addr": "192.168.1.10:12000",
    "ws_addr": "192.168.1.10:8080",
    "data_dir": "./data/node-1",
    "peers": [
        "192.168.1.10:12000",
        "192.168.1.11:12000",
        "192.168.1.12:12000"
    ],
    "bootstrap": true
}
```
> [!WARNING]
> **Bootstrap:** Only **ONE** person (the first one to start) should have `"bootstrap": true` in their config. Everyone else must have `false`.

---

## 4. Running the Demo

1. **Start Node 1 (The Initial Leader):**
   ```powershell
   ./server.exe -config config.json
   ```
2. **Start Node 2 & 3:**
   Ensure their `bootstrap` is `false`, then run the same command.
3. **Open the Editor:**
   Go to `http://YOUR_IP:8080` in a browser.
   > [!TIP]
   > The editor now **automatically detects** the laptop's IP address. You don't need to manually change the `ws://...` value in the browser anymore; it will default to the laptop you are currently accessing.

---

## 5. Chaos Testing Scenarios (For the Professor)

### Scenario 1: Kill a Follower
1. Identity which node is the leader (the terminal will say "became leader").
2. Choose a **Follower** and press `Ctrl+C`.
3. **Observation:** The other two laptops can still edit and sync. Convergence is maintained.

### Scenario 2: Kill the Leader (Election Test)
1. Kill the **Leader** node.
2. **Observation:** One of the followers will time out and start an election. A new leader will be chosen.
3. Refresh the browser on your laptop—it should redirect to the new leader's IP.

### Scenario 3: Network Partition (Isolation Test)
1. Turn off the Wi-Fi on one laptop.
2. Make edits on that laptop (they will be local-only) and edits on the other two.
3. Turn Wi-Fi back on.
4. **Observation:** The isolated node will receive the missing revisions and merge its local changes back into the cluster.

---

## Cleanup (After the Exam)
To remove the firewall rule:
```powershell
Remove-NetFirewallRule -DisplayName "DistributedEditor"
```
