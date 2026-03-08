## Public Cloud Managed Kubernetes — Egress Proxy Strategy

Target platforms: **GKE**, **EKS**, **AKS**

---

## Key constraint: node access varies by cluster mode

Before choosing an architecture, determine which cluster mode the customer uses:

| Platform | Standard/Managed mode | Autopilot/Serverless mode |
|---|---|---|
| GKE | GKE Standard — full node access | GKE Autopilot — nodes fully managed by Google |
| EKS | EKS Standard — full node access | EKS Fargate — no node access |
| AKS | AKS Standard — full node access | AKS Automatic — nodes managed by Azure |

In **Autopilot/Fargate/Automatic** modes, no DaemonSet can touch CNI configuration.
**Custom CNI and Istio CNI are both blocked.** Only application-level or platform-native approaches work.

---

## CNI binary path — platform differences

Every managed cloud platform uses a different CNI binary directory.
This affects both Istio CNI and the custom `egress-proxy` CNI DaemonSet:

| Platform | CNI bin dir | CNI conf dir |
|---|---|---|
| GKE Standard | `/home/kubernetes/bin` | `/etc/cni/net.d` |
| EKS Standard | `/opt/cni/bin` | `/etc/cni/net.d` |
| AKS Standard | `/opt/cni/bin` | `/etc/cni/net.d` |
| kind (dev) | `/opt/cni/bin` | `/etc/cni/net.d` |

> The custom CNI DaemonSet currently hardcodes `/host/opt/cni/bin`.
> This **will fail on GKE**. `EGRESS_CNI_BIN_DIR` must be made configurable via env var.

---

## Architecture decision tree

```
Is the customer on Autopilot / Fargate / AKS Automatic?
  YES → CNI is blocked entirely
        → Use Tier 1 (HTTP_PROXY) + Tier 2 (platform-native egress)
        → restricted PSA is achievable without CNI

  NO (Standard nodes)
        → Does the customer already have Istio installed?
            YES → Use Istio CNI (already supported in this repo)
            NO  → Does the customer accept a new DaemonSet?
                    YES → Deploy custom egress-proxy CNI DaemonSet
                    NO  → Fall back to Tier 1 + Tier 2
```

---

## Tier 1 — Application-level proxy (works everywhere, zero privileges)

Set proxy environment variables on the app container.
No cluster permissions required. Compatible with `restricted` PSA.

```yaml
env:
  - name: HTTP_PROXY
    value: "http://proxy.proxy.svc.cluster.local:8080"
  - name: HTTPS_PROXY
    value: "http://proxy.proxy.svc.cluster.local:8080"
  - name: NO_PROXY
    value: "localhost,127.0.0.1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
```

Combine with a NetworkPolicy that fails closed:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: egress-proxy-only
  namespace: app
spec:
  podSelector: {}
  policyTypes:
    - Egress
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: proxy
      ports:
        - port: 8080
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
      ports:
        - port: 53
          protocol: UDP
```

> ✅ Works on all platforms including Autopilot/Fargate
> ✅ `restricted` PSA compatible
> ✅ Zero cluster privileges required
> ⚠️ Not transparent — app must respect HTTP_PROXY

---

## Tier 2 — Platform-native egress gateway (transparent, no custom components)

Each cloud platform has a native mechanism to redirect or restrict egress at the network level.
The customer configures it using tools they already own and trust.

### GKE — Cloud NAT + VPC Firewall

Route all pod egress through a Cloud NAT gateway, then firewall everything except the proxy:

```
GKE pod egress
  → Cloud NAT (managed by GCP, no CNI change)
  → VPC firewall rule: allow only proxy IP on egress
  → proxy forwards to internet
```

> ✅ Fully managed by GCP — no DaemonSet, no CNI change
> ✅ `restricted` PSA compatible
> ✅ Enforced at VPC level — cannot be bypassed by the pod
> ⚠️ Requires GCP network-level configuration outside Kubernetes

### GKE — Dataplane V2 (Cilium-based) EgressPolicy

GKE Dataplane V2 uses Cilium under the hood and supports `CiliumEgressGatewayPolicy`:

```yaml
apiVersion: cilium.io/v2
kind: CiliumEgressGatewayPolicy
metadata:
  name: egress-proxy-policy
spec:
  selectors:
    - podSelector:
        matchLabels:
          app: workspace
  destinationCIDRs:
    - "0.0.0.0/0"
  egressGateway:
    nodeSelector:
      matchLabels:
        egress-gateway: "true"
    egressIP: 10.0.0.5   # your proxy node IP
```

> ✅ No custom CNI required — Cilium is GKE's own dataplane
> ✅ `restricted` PSA compatible
> ✅ Transparent — no app changes
> ⚠️ Only available with GKE Dataplane V2 enabled

### EKS — VPC CNI NetworkPolicy + AWS NAT Gateway

Use EKS native NetworkPolicy (built into VPC CNI v1.14+) to restrict egress, combined with a NAT Gateway:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: egress-proxy-only
  namespace: app
spec:
  podSelector: {}
  policyTypes:
    - Egress
  egress:
    - to:
        - ipBlock:
            cidr: 10.0.1.5/32   # proxy pod IP or NLB front of proxy
```

At the VPC level, add a security group rule allowing outbound only to the proxy.

> ✅ Uses AWS-native tooling, no extra components
> ✅ `restricted` PSA compatible
> ⚠️ IP-based, not namespace-based — proxy IP must be stable (use a Service with fixed ClusterIP)

### AKS — Azure CNI + Network Security Group

Azure CNI supports NetworkPolicy natively. Combine with an NSG rule at the VNet level:

```yaml
# Same Kubernetes NetworkPolicy as above
# + Azure NSG rule: allow egress only to proxy subnet
```

> ✅ Azure-native, no extra components
> ✅ `restricted` PSA compatible
> ⚠️ Requires coordination with Azure network team

---

## Tier 3 — Full transparent interception (CNI-level, restricted PSA, requires DaemonSet)

### Option A — Istio CNI (if customer already has Istio)

If Istio is already installed, enable the CNI plugin:

```bash
# GKE — note the non-standard CNI bin path
helm upgrade --install istio-cni istio/cni \
  --namespace kube-system \
  --version 1.23.0 \
  --set cni.cniBinDir=/home/kubernetes/bin \   # GKE-specific
  --values k8s/istio/values-cni.yaml

# EKS / AKS — standard path
helm upgrade --install istio-cni istio/cni \
  --namespace kube-system \
  --version 1.23.0 \
  --values k8s/istio/values-cni.yaml
```

> ✅ `restricted` PSA compatible
> ✅ Fully transparent — no app changes
> ✅ Trusted by most security teams (Red Hat, Google, Microsoft all support Istio)
> ⚠️ Requires cluster-admin to install
> ⚠️ Customer must not already be on Autopilot/Fargate

### Option B — Custom egress-proxy CNI DaemonSet

Deploy the `egress-proxy-cni` DaemonSet from this repo.
Functionally identical to Istio CNI but minimal — does exactly one thing.

Must set the correct CNI bin dir per platform:

```yaml
# k8s/cni-plugin/daemonset.yaml
env:
  - name: EGRESS_CNI_BIN_DIR    # must be made configurable — currently hardcoded
    value: "/home/kubernetes/bin"   # GKE
  # value: "/opt/cni/bin"           # EKS, AKS
```

> ✅ `restricted` PSA compatible
> ✅ Minimal footprint — no service mesh overhead
> ✅ Transparent — no app changes
> ⚠️ Requires cluster-admin
> ⚠️ Less trusted than Istio CNI (unknown third-party binary)
> ⚠️ CNI bin path must be configured per platform
> ⚠️ Does not work on Autopilot/Fargate

---

## Platform support matrix

| Approach | GKE Standard | GKE Autopilot | EKS Standard | EKS Fargate | AKS Standard | AKS Automatic | restricted PSA |
|---|---|---|---|---|---|---|---|
| HTTP_PROXY + NetworkPolicy | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Cloud-native egress (NAT/NSG) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |
| Dataplane V2 / Cilium Egress | ✅ (V2 only) | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ |
| Istio CNI | ✅ | ❌ | ✅ | ❌ | ✅ | ❌ | ✅ |
| Custom CNI DaemonSet | ✅* | ❌ | ✅ | ❌ | ✅ | ❌ | ✅ |
| Init container (NET_ADMIN) | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ baseline only |

*GKE Standard requires setting `EGRESS_CNI_BIN_DIR=/home/kubernetes/bin`

---

## Recommended default installation per platform

```
GKE Autopilot / EKS Fargate / AKS Automatic:
  → HTTP_PROXY env vars + NetworkPolicy (Tier 1)
  → Document Cloud NAT / VPC firewall as optional enhancement (Tier 2)

GKE Standard (Dataplane V2):
  → HTTP_PROXY + NetworkPolicy as baseline
  → Offer CiliumEgressGatewayPolicy as transparent upgrade (Tier 2)
  → Offer Istio CNI if customer already has Istio (Tier 3)

GKE Standard (legacy) / EKS Standard / AKS Standard:
  → HTTP_PROXY + NetworkPolicy as baseline
  → Offer Istio CNI if customer has Istio (Tier 3)
  → Offer custom CNI DaemonSet as lightweight alternative (Tier 3)
```

Never require Tier 3 as a baseline. Privilege escalation must always be opt-in.
