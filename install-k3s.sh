#!/bin/bash

################################################################################
# K3s + Docker Installation Script for AWS EC2 with ECR Access via IAM Instance Profile
# 
# This script installs Docker and k3s on a fresh Amazon Linux 2 or Ubuntu EC2 instance
# and configures it to access AWS ECR using IAM instance profile credentials.
#
# Prerequisites:
# - EC2 instance with IAM instance profile attached
# - Instance profile must have ECR pull permissions:
#   - ecr:GetAuthorizationToken
#   - ecr:BatchGetImage
#   - ecr:GetDownloadUrlForLayer
#
# Usage:
#   ./install-k3s.sh [OPTIONS]
#
# Options:
#   --k3s-version VERSION     Specific k3s version (default: latest stable)
#   --cluster-name NAME       Name for the k3s cluster (default: infrasage)
#   --ecr-registry REGISTRY   AWS ECR registry URL (e.g., 058264174161.dkr.ecr.ap-south-1.amazonaws.com)
#   --aws-region REGION       AWS region (default: ap-south-1)
#   --help                    Show this help message
#
# Example:
#   ./install-k3s.sh \
#     --k3s-version v1.27.0 \
#     --cluster-name infrasage \
#     --ecr-registry 058264174161.dkr.ecr.ap-south-1.amazonaws.com \
#     --aws-region ap-south-1
#
################################################################################

set -euo pipefail

# Color output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
K3S_VERSION="latest"
CLUSTER_NAME="infrasage"
ECR_REGISTRY=""
AWS_REGION="ap-south-1"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Logging functions
log_info() {
    echo -e "${BLUE}[$(date +'%Y-%m-%d %H:%M:%S')]${NC} ${GREEN}ℹ${NC}  $*"
}

log_warn() {
    echo -e "${BLUE}[$(date +'%Y-%m-%d %H:%M:%S')]${NC} ${YELLOW}⚠${NC}  $*" >&2
}

log_error() {
    echo -e "${BLUE}[$(date +'%Y-%m-%d %H:%M:%S')]${NC} ${RED}✗${NC}  $*" >&2
}

log_success() {
    echo -e "${BLUE}[$(date +'%Y-%m-%d %H:%M:%S')]${NC} ${GREEN}✓${NC}  $*"
}

# Parse arguments
parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --k3s-version)
                K3S_VERSION="$2"
                shift 2
                ;;
            --cluster-name)
                CLUSTER_NAME="$2"
                shift 2
                ;;
            --ecr-registry)
                ECR_REGISTRY="$2"
                shift 2
                ;;
            --aws-region)
                AWS_REGION="$2"
                shift 2
                ;;
            --help)
                show_help
                exit 0
                ;;
            *)
                log_error "Unknown option: $1"
                show_help
                exit 1
                ;;
        esac
    done
}

show_help() {
    head -n 32 "$0" | tail -n +2
}

# Detect OS
detect_os() {
    if [ -f /etc/os-release ]; then
        . /etc/os-release
        OS=$ID
        VERSION=$VERSION_ID
    else
        log_error "Cannot detect OS"
        exit 1
    fi
    
    log_info "Detected OS: $OS (Version: $VERSION)"
}

# Install dependencies based on OS
install_dependencies() {
    log_info "Installing dependencies..."
    
    case $OS in
        amzn|amazonlinux)
            yum update -y
            yum install -y \
                wget \
                git \
                tar \
                gzip \
                ca-certificates \
                awscli \
                jq \
                htop
            ;;
        ubuntu|debian)
            apt-get update
            apt-get install -y \
                wget \
                git \
                tar \
                gzip \
                ca-certificates \
                awscli \
                jq \
                htop
            ;;
        *)
            log_error "Unsupported OS: $OS"
            exit 1
            ;;
    esac
    
    log_success "Dependencies installed"
}

# Install Docker
install_docker() {
    log_info "Installing Docker..."

    if command -v docker &> /dev/null; then
        log_info "Docker already installed: $(docker --version)"
        systemctl enable docker 2>/dev/null || true
        systemctl start  docker 2>/dev/null || true
        return 0
    fi

    case $OS in
        amzn|amazonlinux)
            yum install -y docker
            ;;
        ubuntu|debian)
            apt-get install -y \
                apt-transport-https \
                gnupg \
                lsb-release
            install -m 0755 -d /etc/apt/keyrings
            curl -fsSL https://download.docker.com/linux/ubuntu/gpg \
                | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
            chmod a+r /etc/apt/keyrings/docker.gpg
            echo \
                "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" \
                > /etc/apt/sources.list.d/docker.list
            apt-get update
            apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
            ;;
        *)
            log_error "Unsupported OS for Docker installation: $OS"
            exit 1
            ;;
    esac

    systemctl enable docker
    systemctl start docker

    log_success "Docker installed: $(docker --version)"
}

# Disable swap (k3s requirement)
disable_swap() {
    log_info "Disabling swap..."
    
    swapoff -a || true
    
    # Comment out swap entries in fstab
    sed -i '/swap/d' /etc/fstab || true
    
    log_success "Swap disabled"
}

# Configure kernel parameters for containerd
configure_kernel_params() {
    log_info "Configuring kernel parameters..."
    
    cat > /etc/sysctl.d/99-k3s.conf <<EOF
# Kubernetes/containerd kernel parameters
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward = 1
net.ipv4.conf.all.rp_filter = 0
net.ipv4.conf.default.rp_filter = 0
net.ipv6.conf.all.forwarding = 1
vm.overcommit_memory = 1
kernel.panic = 10
kernel.panic_on_oops = 1
EOF
    modprobe br_netfilter
    sysctl -p /etc/sysctl.d/99-k3s.conf
    
    log_success "Kernel parameters configured"
}

# Install k3s
install_k3s() {
    log_info "Installing k3s (version: $K3S_VERSION)..."
    
    # Skip if already installed and running at the requested version
    if command -v k3s &>/dev/null && systemctl is-active --quiet k3s; then
        INSTALLED_VERSION=$(k3s --version | awk '{print $3}')
        if [ "$K3S_VERSION" = "latest" ] || [ "$INSTALLED_VERSION" = "$K3S_VERSION" ]; then
            log_info "k3s already installed and running: $INSTALLED_VERSION"
            return 0
        fi
        log_info "k3s version mismatch (installed: $INSTALLED_VERSION, requested: $K3S_VERSION) — upgrading..."
    fi
    
    # Create k3s directory
    mkdir -p /etc/rancher/k3s
    
    # Install k3s
    if [ "$K3S_VERSION" = "latest" ]; then
        curl -sfL https://get.k3s.io | sh -
    else
        curl -sfL https://get.k3s.io | INSTALL_K3S_VERSION="$K3S_VERSION" sh -
    fi
    
    # Wait for k3s to be ready
    log_info "Waiting for k3s to be ready..."
    sleep 10
    
    # Check if k3s is running
    if systemctl is-active --quiet k3s; then
        log_success "k3s installed and running"
    else
        log_error "k3s failed to start"
        systemctl status k3s || true
        exit 1
    fi
}

# Configure ECR access for k3s
configure_ecr_access() {
    log_info "Configuring ECR access..."
    
    if [ -z "$ECR_REGISTRY" ]; then
        log_warn "ECR_REGISTRY not provided, skipping ECR configuration"
        return 0
    fi
    
    # Create k3s containerd config directory
    mkdir -p /etc/rancher/k3s
    
    # Get AWS account ID and region from instance metadata
    TOKEN=$(curl -X PUT "http://169.254.169.254/latest/api/token" -H "X-aws-ec2-metadata-token-ttl-seconds: 21600")
    AWS_ACCOUNT_ID=$(echo "$ECR_REGISTRY" | cut -d'.' -f1)
    
    log_info "AWS Account ID: $AWS_ACCOUNT_ID"
    log_info "AWS Region: $AWS_REGION"
    log_info "ECR Registry: $ECR_REGISTRY"
    
    # Create containerd configuration for ECR authentication
    cat > /etc/rancher/k3s/registries.yaml <<EOF
mirrors:
  ${ECR_REGISTRY}:
    endpoint:
      - https://${ECR_REGISTRY}

configs:
  ${ECR_REGISTRY}:
    auth:
      username: AWS
      password: ""
EOF
    
    log_success "ECR configuration created at /etc/rancher/k3s/registries.yaml"
}

# Configure k3s to use containerd config
configure_containerd() {
    log_info "Configuring containerd..."
    
    # Create k3s config directory
    mkdir -p /etc/rancher/k3s
    
    # Create k3s configuration to use registries.yaml
    cat > /etc/rancher/k3s/config.yaml <<EOF
# K3s Configuration
data-dir: /var/lib/rancher/k3s
cluster-name: ${CLUSTER_NAME}

# Enable servicelb and traefik
disable:
  - servicelb
  - traefik

# Configure kubelet
kubelet-arg:
  - max-pods=250
  - max-open-files=2000000

# Container runtime configuration
container-runtime-cgroup-driver: systemd

# Enable metrics server
metrics-server-arg:
  - --kubelet-insecure-tls=true
  - --kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname

# kube-proxy configuration
kube-proxy-arg:
  - --proxy-mode=iptables
  - --cluster-cidr=10.42.0.0/16
  - --service-cidr=10.43.0.0/16
EOF
    
    log_success "K3s configuration created"
}

# Setup kubeconfig
setup_kubeconfig() {
    log_info "Setting up kubeconfig..."
    
    # Wait for k3s to generate kubeconfig
    for i in {1..30}; do
        if [ -f /etc/rancher/k3s/k3s.yaml ]; then
            break
        fi
        log_info "Waiting for kubeconfig... ($i/30)"
        sleep 1
    done
    
    if [ ! -f /etc/rancher/k3s/k3s.yaml ]; then
        log_error "Kubeconfig not found"
        exit 1
    fi
    
    # Copy kubeconfig to standard location
    mkdir -p /root/.kube
    cp /etc/rancher/k3s/k3s.yaml /root/.kube/config
    chmod 600 /root/.kube/config
    
    # Replace localhost with actual IP for remote access
    INSTANCE_IP=$(curl -s http://169.254.169.254/latest/meta-data/local-ipv4)
    sed -i "s/127.0.0.1:6443/${INSTANCE_IP}:6443/g" /root/.kube/config
    
    log_success "Kubeconfig configured at /root/.kube/config"
    log_info "Cluster IP for remote access: ${INSTANCE_IP}:6443"
}

# Install kubectl if not already present
install_kubectl() {
    log_info "Installing kubectl..."
    
    if command -v kubectl &> /dev/null; then
        log_info "kubectl already installed: $(kubectl version --client --short 2>/dev/null || echo 'version check failed')"
        return 0
    fi
    
    # Link k3s kubectl
    ln -sf /usr/local/bin/k3s /usr/local/bin/kubectl
    
    log_success "kubectl linked to k3s"
}

# Install Helm (optional but useful)
install_helm() {
    log_info "Installing Helm..."
    
    if command -v helm &> /dev/null; then
        log_info "Helm already installed: $(helm version --short)"
        return 0
    fi
    
    curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
    
    log_success "Helm installed"
}

# Verify installation
verify_installation() {
    log_info "Verifying installation..."
    
    # Check k3s service
    if ! systemctl is-active --quiet k3s; then
        log_error "k3s service is not running"
        exit 1
    fi
    
    # Check nodes
    if ! kubectl get nodes &>/dev/null; then
        log_error "Cannot access Kubernetes cluster"
        exit 1
    fi
    
    NODES=$(kubectl get nodes --no-headers 2>/dev/null | wc -l)
    log_success "K3s cluster has $NODES node(s)"
    
    # Check kube-system namespace
    KUBE_SYSTEM_PODS=$(kubectl get pods -n kube-system --no-headers 2>/dev/null | wc -l)
    log_success "kube-system namespace has $KUBE_SYSTEM_PODS pod(s)"
    
    # Show node details
    log_info "Node details:"
    kubectl get nodes -o wide
}

# Create namespace for infrasage
create_namespace() {
    log_info "Creating infrasage namespace..."
    
    kubectl create namespace infrasage --dry-run=client -o yaml | kubectl apply -f -
    
    log_success "Infrasage namespace created"
}

# Setup IAM authenticator for ECR (using aws-cli)
setup_iam_authenticator() {
    log_info "Setting up IAM ECR token updater..."
    
    if [ -z "$ECR_REGISTRY" ]; then
        log_warn "ECR_REGISTRY not provided, skipping IAM authenticator setup"
        return 0
    fi
    
    # Create a service account for ECR token refresh
    cat > /usr/local/bin/update-ecr-token.sh <<'EOF'
#!/bin/bash
# Update ECR authentication token every 11.5 hours
# (ECR tokens are valid for 12 hours)

set -euo pipefail

ECR_REGISTRY="$1"
AWS_REGION="$2"
KUBECONFIG="${KUBECONFIG:-/etc/rancher/k3s/k3s.yaml}"

while true; do
    # Get ECR authentication token using IAM instance profile
    TOKEN=$(aws ecr get-authorization-token \
        --region "${AWS_REGION}" \
        --query 'authorizationData[0].authorizationToken' \
        --output text)
    
    # Decode token to get password
    PASSWORD=$(echo "$TOKEN" | base64 -d | cut -d: -f2)
    
    # Update kubernetes secret
    kubectl create secret docker-registry ecr-secret \
        --docker-server="https://${ECR_REGISTRY}" \
        --docker-username=AWS \
        --docker-password="${PASSWORD}" \
        --docker-email=notused@example.com \
        -n infrasage \
        --dry-run=client -o yaml | kubectl apply -f -
    
    echo "$(date): ECR token updated successfully"
    
    # Sleep for 11.5 hours before next refresh
    sleep 41400
done
EOF
    
    chmod +x /usr/local/bin/update-ecr-token.sh
    
    # Create systemd service for token refresh
    cat > /etc/systemd/system/ecr-token-updater.service <<EOF
[Unit]
Description=Update ECR Authentication Token
After=k3s.service
Wants=k3s.service

[Service]
Type=simple
ExecStart=/usr/local/bin/update-ecr-token.sh ${ECR_REGISTRY} ${AWS_REGION}
Restart=always
RestartSec=30
StandardOutput=journal
StandardError=journal
SyslogIdentifier=ecr-token-updater
User=root
Environment="KUBECONFIG=/etc/rancher/k3s/k3s.yaml"

[Install]
WantedBy=multi-user.target
EOF
    
    systemctl daemon-reload
    systemctl enable ecr-token-updater.service
    
    # Restart (not just start) so any updated unit file / script is picked up
    sleep 10
    systemctl restart ecr-token-updater.service || true
    
    log_success "ECR token updater service created"
}

# Display summary
display_summary() {
    log_success "═══════════════════════════════════════════════════════════"
    log_success "K3s Installation Complete!"
    log_success "═══════════════════════════════════════════════════════════"
    
    INSTANCE_IP=$(curl -s http://169.254.169.254/latest/meta-data/local-ipv4)
    PUBLIC_IP=$(curl -s http://169.254.169.254/latest/meta-data/public-ipv4 || echo "N/A")
    
    cat <<EOF

📊 Cluster Information:
   Cluster Name:     ${CLUSTER_NAME}
   K3s Version:      $(k3s --version)
   Local IP:         ${INSTANCE_IP}
   Public IP:        ${PUBLIC_IP}
   Kubeconfig:       /root/.kube/config

🔐 ECR Configuration:
   ECR Registry:     ${ECR_REGISTRY:-"Not configured"}
   AWS Region:       ${AWS_REGION}
   Token Updater:    $(systemctl is-active ecr-token-updater.service 2>/dev/null || echo "Disabled")

📋 Useful Commands:
   kubectl get nodes
   kubectl get pods -A
   kubectl get ns
   helm list -A

🔗 Remote Access:
   To access from another machine, copy /root/.kube/config and update:
   server: https://${INSTANCE_IP}:6443

📝 Logs:
   K3s logs:           journalctl -u k3s -f
   ECR token updater:  journalctl -u ecr-token-updater.service -f

EOF
}

# Main execution
main() {
    log_info "Starting K3s installation process..."
    log_info "Script started at $(date)"
    
    parse_args "$@"
    
    log_info "Configuration:"
    log_info "  K3s Version:    $K3S_VERSION"
    log_info "  Cluster Name:   $CLUSTER_NAME"
    log_info "  ECR Registry:   ${ECR_REGISTRY:-'Not configured'}"
    log_info "  AWS Region:     $AWS_REGION"
    
    detect_os
    
    # Check if running as root
    if [ "$EUID" -ne 0 ]; then
        log_error "This script must be run as root"
        exit 1
    fi
    
    install_dependencies
    install_docker
    disable_swap
    configure_kernel_params
    install_k3s
    configure_ecr_access
    configure_containerd
    setup_kubeconfig
    install_kubectl
    install_helm
    create_namespace
    setup_iam_authenticator
    verify_installation
    display_summary
    PASSWORD=$(aws ecr get-login-password --region ap-south-1)
    kubectl create namespace infrasage --dry-run=client -o yaml | kubectl apply -f -
    kubectl create secret docker-registry ecr-secret \
        --docker-server=058264174161.dkr.ecr.ap-south-1.amazonaws.com \
        --docker-username=AWS \
        --docker-password="$PASSWORD" \
        -n infrasage \
        --dry-run=client -o yaml | kubectl apply -f -
    log_success "K3s installation completed successfully!"
}

# Error handler
trap 'log_error "Script failed at line $LINENO"; exit 1' ERR

main "$@"
