#!/bin/bash
# Paste this into "Advanced details -> User data" when launching each EC2
# instance (Amazon Linux 2023). It installs Docker + Compose, adds swap so the
# small free-tier instance can build Go, and downloads the project.
set -eux

dnf install -y docker git
systemctl enable --now docker
usermod -aG docker ec2-user

# Docker Compose plugin
mkdir -p /usr/local/lib/docker/cli-plugins
curl -sSL "https://github.com/docker/compose/releases/download/v2.29.7/docker-compose-linux-$(uname -m)" \
  -o /usr/local/lib/docker/cli-plugins/docker-compose
chmod +x /usr/local/lib/docker/cli-plugins/docker-compose

# 2 GB swap: free-tier instances have only 1 GB RAM
fallocate -l 2G /swapfile && chmod 600 /swapfile && mkswap /swapfile && swapon /swapfile
echo '/swapfile swap swap defaults 0 0' >> /etc/fstab

# CHANGE THIS to your GitHub repo URL
sudo -u ec2-user git clone https://github.com/unnathivinod/flash-sale-system.git /home/ec2-user/flash-sale-system
