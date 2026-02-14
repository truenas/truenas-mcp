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

## Snapshots

- "Show me all snapshots in the tank pool"
- "What are the 20 most recent snapshots?"
- "List snapshots for the tank/shares/data dataset"
- "Show me snapshots that have holds"
- "What snapshots exist for my important datasets?"
- "List snapshots created by automatic snapshot tasks"

## Virtual Machines

- "What VMs are currently running?"
- "Show me all virtual machines"
- "List VMs that are set to autostart"
- "What's the memory allocation for my VMs?"
- "Show me details for the homeassistant VM"
- "Which VMs are stopped?"

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
  - *This will trigger a multi-step guided wizard that:*
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
- "I need an NFS share restricted to my 192.168.1.0/24 network"
- "Create an NFS share with root squashing for security"
- "Share my data directory with specific hosts only"

## Task Management

- "Show me all active tasks"
- "What's the status of task abc123?"
- "Check the progress of my app upgrade"

## Dry-Run Mode

- "Show me what would happen if I upgrade the plex app"
- "Preview the changes before upgrading nextcloud"
