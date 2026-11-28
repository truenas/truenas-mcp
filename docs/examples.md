# TrueNAS MCP Example Queries

This document provides example natural language queries you can use with TrueNAS MCP through an AI assistant like Claude.

## System Information

- "What version of TrueNAS is running?"
- "Are there any system alerts?"
- "What's the system health status?"
- "Are there any active jobs or tasks running?"

## Jobs & Tasks

- "Show me all running jobs"
- "Are there any replications in progress?"
- "What tasks have completed recently?"

## Storage

- "Show me all storage pools and their health status"
- "What are the top 10 datasets using the most space?"
- "Which datasets are encrypted?"
- "Show me datasets in the tank pool"
- "List datasets sorted by available space"
- "What's taking up space in my replications dataset?"
- "What SMB shares are configured?"
- "Why can't exampleuser open the TeamDocs share?"
- "Show the permissions on /mnt/tank/shares/docs"
- "Give the staff group modify access to /mnt/tank/shares/docs, keeping the owner's full control (preview first)"
- "Apply that ACL to everything under /mnt/tank/shares/docs, and show me how much it touches first"
- "Upload the installer ISO from https://example.com/isos/installer.iso to /mnt/tank/isos/installer.iso; its SHA-256 is <checksum>"
- "Copy ~/Downloads/isos/installer.iso from this machine to /mnt/tank/isos (preview first; needs --upload-source-dirs to include that folder)"

## Disks and Services

- "Is any disk failing or running hot?"
- "Which disks are not in a pool?"
- "Which disk in tank has checksum errors, and what is its serial?"
- "Take sdb in tank offline so I can reseat it (preview first)"
- "Bring sdb in tank back online"
- "Replace the failed disk in tank with an unused disk of the same size or larger; show me the preview first"
- "Is SSH running, and does it start at boot?"
- "Restart the SMB service (preview first)"
- "Stop the FTP service and make sure it does not start at boot"

## Snapshots

- "Show me all snapshots in the tank pool"
- "What are the 20 most recent snapshots?"
- "List snapshots for the tank/shares/data dataset"
- "Show me snapshots that have holds"
- "What snapshots exist for my important datasets?"
- "List snapshots created by automatic snapshot tasks"

## Replication and Snapshot Tasks

- "Show me my replication tasks and when each last ran"
- "Did the last replication of tank/data succeed?"
- "Which periodic snapshot task feeds replication task 1?"
- "Does my recursive snapshot task on tank/data cover every child dataset?"
- "Snapshot tank/data before I change it"
- "Clone tank/data@manual-1 to tank/restore/data so I can copy a file back"
- "Hold tank/data@manual-1 so retention does not delete it"
- "Release the hold on tank/data@manual-1"
- "Roll tank/data back to its latest snapshot"
- "What would rolling tank/data back to tank/data@manual-1 destroy?"
- "Delete tank/data@manual-1"
- "Delete the snapshot tank/data@before-upgrade on tank/data and all its children"
- "Run snapshot task 3 now and show me the snapshots it made"
- "Replicate tank/data off-site now"

## Virtual Machines

- "What VMs are currently running?"
- "Show me all virtual machines"
- "List VMs that are set to autostart"
- "What's the memory allocation for my VMs?"
- "Show me details for the homeassistant VM"
- "What IP addresses does VM 2 have?"
- "Which VMs are stopped?"
- "Create a UEFI VM named lab01 with 2 vCPUs, 4096 MiB RAM, a new 32 GiB VIRTIO zvol at tank/vms/lab01, the installer ISO at /mnt/tank/isos/installer.iso, a VIRTIO NIC on br0, and a local SPICE display (preview first)"
- "Start the lab01 VM"
- "Gracefully stop the lab01 VM and track the stop job"
- "Forcibly power off the lab01 VM (preview first)"
- "Restart the lab01 VM"
- "Change the boot order of device 10 on lab01 to 1001 so it boots from disk first (preview first)"
- "Eject the installer CDROM (device 11) from the stopped lab01 VM without deleting the ISO file"
- "Swap the CDROM ISO on device 11 to /mnt/tank/isos/rescue.iso (preview first)"
- "Delete the stopped lab01 VM while keeping its backing zvols (preview first)"
- "Delete the stopped lab01 VM and permanently destroy its attached zvols (preview first)"
- "Open a serial console to the running lab01 VM (requires `--allow-vm-console=lab01`), wait for the login prompt, and close the console when done"
- "Read the current screen of the open VM serial console and wait up to 30 seconds for 'Select language' to appear"
- "Send Down, Down, Enter on the open VM serial console to choose the third installer menu option"

## Performance

- "Show me CPU and memory usage over the past day"
- "What's the network traffic on the main interface?"
- "Show me disk I/O metrics for the past week"

## Capacity Planning

- "How near to CPU capacity is my TrueNAS?"
- "Analyze system capacity over the past 90 days"
- "What's my current storage pool utilization?"
- "Show me detailed capacity information for the tank pool"
- "Are there any capacity warnings I should be aware of?"
- "Based on current trends, when should I plan to expand?"

## Applications

### Installed Apps
- "What apps are installed and running?"
- "Are there any app updates available?"
- "Upgrade the plex app to the latest version"
- "Delete the jellyfin app (show me what will happen first)"
- "Remove the plex app and its container images"

### App Catalog & Installation
- "Search for media server apps in the catalog"
- "What apps are available for home automation?"
- "Tell me about the Plex app"
- "Show me details about Nextcloud"
- "What storage does Jellyfin need?"
- "Install Plex on my system"
  - *A typical install follows the workflow in the tool descriptions:*
    - *Searches catalog for the app*
    - *Gets app details and storage requirements*
    - *Queries available pools*
    - *Proposes dataset structure (e.g., tank/apps/plex/config, tank/apps/plex/data)*
    - *Creates missing datasets with appropriate quotas*
    - *Validates configuration*
    - *Previews installation with dry-run*
    - *Executes installation and tracks progress*
- "Set up Nextcloud with 1TB storage on my tank pool"
- "Install Jellyfin using my existing media dataset"

## Directory Services

- "Is Active Directory configured?"
- "What's the directory service status?"
- "Show me the directory service configuration"
- "Join the domain corp.example.com with username admin"
- "Configure LDAP at ldap.example.com with bind DN cn=admin,dc=example,dc=com"
- "Set up FreeIPA integration for ipa.example.com"
- "Leave the current directory service domain (dry run first)"
- "Disable the directory service without leaving the domain"
- "Refresh the user and group cache from Active Directory"
- "What certificates are available for LDAP authentication?"
- "Is my Active Directory connection healthy?"

## Users and Groups

- "Which local users have SMB access?"
- "Create a user carol, Carol Smith, with a random password and SMB access"
- "Create user dan with a home directory under /mnt/tank/home and add him to the staff group"
- "Lock the user bob"
- "Set a new random password for alice"
- "Create a group media with members alice and bob"
- "Remove bob from the staff group"
- "Delete the user carol; keep her home directory and primary group"
- "Delete the group media"

## System Updates

- "Check if there are any TrueNAS system updates available"
- "What version of TrueNAS am I running?"
- "Download the latest TrueNAS system update"
- "What's the status of my system update?"
- "Apply the downloaded system update"
- "Apply the update and reboot the system"
- "Reboot the TrueNAS system"

## Managing Boot Environments

- "Which boot environments do I have?"
- "What boot environment am I currently running?"
- "Show me boot environments that are safe to delete"
- "How much space are boot environments using?"
- "Delete boot environment '23.10-MASTER-20231015-120000' (dry run first)"
- "Delete boot environment '23.10-MASTER-20231015-120000'"
- "Show me the 10 oldest boot environments"
- "Which boot environments are protected?"

## Managing Pool Scrubs

- "What's the scrub status of my pools?"
- "When was tank last scrubbed?"
- "Show me all scrub schedules"
- "Is there a scrub running on tank?"
- "Create a weekly scrub schedule for tank on Sunday at 2am"
- "Create a monthly scrub for flash on the 1st at 3am"
- "Start a scrub on tank"
- "Check progress of my scrub"
- "Delete the scrub schedule for flash"
- "How often should I scrub my pools?"
- "What's the recommended scrub frequency for home use?"

## Dataset Creation

- "Create a new dataset for file sharing"
- "Create an encrypted dataset with a 500GB quota"
- "Set up a dataset optimized for SMB shares"
- "Create a dataset in the tank pool for my documents"
- "I need a dataset with LZ4 compression for app storage"

## SMB Share Creation

- "Create a new SMB share for my team"
- "Set up a Time Machine share for macOS backups"
- "Create a read-only share for archives"
- "I want to share my photos with Windows clients"
- "Set up an encrypted SMB share with access restrictions"
- "Create a share that's only accessible from my local network"

## NFS Share Creation

- "Create an NFS share for my Linux servers"
- "Set up a read-only NFS export for backups"
- "I need an NFS share restricted to my 192.0.2.0/24 network"
- "Create an NFS share with root squashing for security"
- "Share my data directory with specific hosts only"

## Changing and Deleting Shares

- "Make the docs SMB share read-only (dry run first)"
- "Rename the SMB share docs to TeamDocs"
- "Move the docs share to /mnt/tank/shares/team"
- "Delete the SMB share old-scans; keep the files"
- "Restrict NFS share 1 to the 192.0.2.0/24 network"
- "Map root to nobody on NFS share 1"
- "Delete NFS share 2"
- "Show the share ACL of docs, then give the staff group FULL and everyone READ"

## Task Management

- "Show me all active tasks"
- "What's the status of task abc123?"
- "Check the progress of my app upgrade"

## Dry-Run Mode

- "Show me what would happen if I upgrade the plex app"
- "Preview the changes before upgrading nextcloud"
