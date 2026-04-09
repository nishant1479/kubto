#!/bin/bash
export PATH=$PATH:$HOME/.local/bin

if ! docker ps > /dev/null 2>&1; then
    echo "Docker permission denied. You will see a graphical prompt asking for your password."
    echo "This will grant temporary access to /var/run/docker.sock so we can spin up your cluster."
    pkexec chmod 666 /var/run/docker.sock || { echo "Failed to get docker permissions"; exit 1; }
fi

if ! kind get clusters | grep -q "kuber-scale-env"; then
    echo "Creating kind cluster 'kuber-scale-env'..."
    kind create cluster --name kuber-scale-env
else
    echo "Kind cluster 'kuber-scale-env' already exists."
fi

echo "Environment is ready! You can now run the autoscaler."
