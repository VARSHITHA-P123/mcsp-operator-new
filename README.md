# MCSP Operator

A Kubernetes Operator built with Go and Kubebuilder that automates customer instance provisioning and deprovisioning on the MultiCloud SaaS Platform (MCSP).

## What it does

When a customer instance is created, the operator automatically:
- Creates a dedicated namespace for the customer using RHACM Policy
- Sets up ResourceQuota and LimitRange on the namespace
- Configures RBAC for image pulling
- Binds the policy to the target cluster using PlacementBinding
- Waits for the namespace to be ready
- Deploys all microservices into the customer namespace via a Kubernetes Job
- Updates the CR status with deployment state and URL

When a customer instance is deleted, the operator automatically cleans up in order:
- Deletes the deployment Job
- Removes the PlacementBinding
- Removes the RHACM Policy
- Deletes the customer namespace and everything inside it

## Key Features

- Parallel onboarding and offboarding of up to 6 customers simultaneously
- Finalizer-based cleanup guarantees no resources are left behind
- Real-time status updates on the CR
- Fast reconciliation with no blocking sleep calls

## Tech Stack

- Go
- Kubebuilder v3.14.0
- controller-runtime
- OpenShift / Kubernetes
- RHACM (Red Hat Advanced Cluster Management)

## Project Structure

```
mcsp-operator-new/
├── api/v1/
│   └── mcspcustomer_types.go        # CR type definitions
├── internal/controller/
│   └── mcspcustomer_controller.go   # Reconciler logic
├── config/
│   ├── crd/                         # CRD yamls
│   ├── rbac/                        # RBAC roles
│   ├── manager/                     # Operator deployment
│   └── samples/                     # Sample CRs
├── deployer/
│   └── Dockerfile                   # Deployer image
└── cmd/
    └── main.go                      # Entry point
```

## MCSPCustomer CR

```yaml
apiVersion: mcsp.mcsp.io/v1
kind: MCSPCustomer
metadata:
  name: tesla
  namespace: mcsp-operator-new-system
spec:
  customerName: tesla
```

