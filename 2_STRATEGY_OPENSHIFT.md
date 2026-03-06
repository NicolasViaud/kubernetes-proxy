## OpenShift native egress mechanisms

### 1 — EgressIP (OVN-Kubernetes native)

Makes all traffic from a namespace appear to come from a specific IP — then you route that IP to your proxy at the network level:

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: cde-egress
spec:
  egressIPs:
    - 192.168.1.100          # dedicated egress IP for your namespace
  namespaceSelector:
    matchLabels:
      egress-controlled: "true"
  podSelector:               # optional: scope to specific pods
    matchLabels:
      app: workspace
```

Then at the **network level** (outside Kubernetes), route `192.168.1.100` to your proxy component.

> ✅ Native OpenShift, no third-party components
> ✅ `restricted` SCC compatible
> ✅ Security team approves it — it's Red Hat's own feature
> ⚠️ Redirection happens at network/router level, not inside the cluster

---

### 2 — EgressFirewall (OVN-Kubernetes native)

Block all egress except to your proxy — the OpenShift equivalent of NetworkPolicy for egress:

```yaml
apiVersion: k8s.ovn.org/v1
kind: EgressFirewall
metadata:
  name: restrict-egress
  namespace: cde-workspaces
spec:
  egress:
    - type: Allow
      to:
        cidrSelector: 10.0.0.5/32    # your proxy component IP
    - type: Allow
      to:
        cidrSelector: 10.96.0.0/12   # cluster service network (DNS etc)
    - type: Deny
      to:
        cidrSelector: 0.0.0.0/0      # block everything else
```

> ✅ Native OpenShift
> ✅ No privileged components
> ✅ Enforced at OVN level — cannot be bypassed by pod
> ❌ Blocks, doesn't redirect — same limitation as NetworkPolicy

---

### 3 — EgressIP + EgressFirewall combined

This is the OpenShift-native equivalent of what you want:

```
your pod
  → OVN forces traffic out via EgressIP 192.168.1.100
  → EgressFirewall blocks everything except your proxy
  → network router sends 192.168.1.100 traffic to your proxy
  → your proxy forwards to internet (or not)
```

```yaml
# Step 1 — label your namespace
apiVersion: v1
kind: Namespace
metadata:
  name: cde-workspaces
  labels:
    egress-controlled: "true"

---
# Step 2 — assign EgressIP
apiVersion: k8s.ovn.org/v1
kind: EgressIP
metadata:
  name: cde-egress
spec:
  egressIPs:
    - 192.168.1.100
  namespaceSelector:
    matchLabels:
      egress-controlled: "true"

---
# Step 3 — block everything except proxy
apiVersion: k8s.ovn.org/v1
kind: EgressFirewall
metadata:
  name: cde-firewall
  namespace: cde-workspaces
spec:
  egress:
    - type: Allow
      to:
        cidrSelector: 10.0.0.5/32    # your proxy
    - type: Deny
      to:
        cidrSelector: 0.0.0.0/0
```

---

### 4 — OpenShift Service Mesh (if approved)

If the customer has OpenShift Service Mesh installed (Red Hat's Istio distribution):

```
OpenShift Service Mesh
  → includes Istio CNI plugin
  → but it's Red Hat supported and approved
  → goes through normal OpenShift operator lifecycle
  → security team much more likely to accept it
```

```bash
# Installed via OperatorHub — fully supported by Red Hat
oc apply -f - <<EOF
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: servicemeshoperator
  namespace: openshift-operators
spec:
  channel: stable
  name: servicemeshoperator
  source: redhat-operators
  sourceNamespace: openshift-marketplace
EOF
```

> ✅ Same Istio CNI mechanism
> ✅ Red Hat supported → security team much more comfortable
> ✅ Installs via standard OpenShift operator lifecycle
> ⚠️ Still requires customer to install it

---

## OpenShift egress mechanisms compared

| Mechanism | Redirects | Blocks | Privileged | Native OCP | Transparent |
|---|---|---|---|---|---|
| EgressIP | ⚠️ At network level | ❌ | ❌ | ✅ | ⚠️ Partial |
| EgressFirewall | ❌ | ✅ | ❌ | ✅ | ❌ |
| EgressIP + EgressFirewall | ⚠️ At network level | ✅ | ❌ | ✅ | ⚠️ Partial |
| OpenShift Service Mesh | ✅ | ✅ | ✅ node-level | ✅ supported | ✅ |
| Istio CNI (upstream) | ✅ | ✅ | ✅ node-level | ❌ | ✅ |

---

## The pattern emerging across all CNIs

Looking at everything we've discussed, a clear pattern emerges:

```
Every CNI/platform has the same two tiers:

Tier 1 — Native, no privileges, block-only
  Kubernetes:  NetworkPolicy
  OpenShift:   EgressFirewall
  Cilium:      CiliumNetworkPolicy
  Calico:      GlobalNetworkPolicy

Tier 2 — Native, node-level privilege, full redirect
  Kubernetes:  Istio CNI plugin
  OpenShift:   OpenShift Service Mesh (Istio CNI)
  Cilium:      CiliumEgressGatewayPolicy
  Calico:      Calico Enterprise EgressGateway
```

---

## Practical recommendation for your multi-customer product

Given everything, the cleanest approach for shipping to diverse customers including OpenShift is:

```
Your default install:
  → HTTP_PROXY env vars          (application layer)
  → EgressFirewall/NetworkPolicy (hard block, platform native)

Your documentation provides platform-specific enhancements:
  ┌─────────────────┬────────────────────────────────┐
  │ Platform        │ Enhanced redirect mechanism     │
  ├─────────────────┼────────────────────────────────┤
  │ OpenShift       │ EgressIP + EgressFirewall       │
  │ Cilium          │ CiliumEgressGatewayPolicy       │
  │ Calico Ent.     │ Calico EgressGateway            │
  │ Any (approved)  │ OpenShift SM / Istio CNI plugin │
  └─────────────────┴────────────────────────────────┘
```

This way you **never require privileged access** as a baseline, but offer progressively stronger guarantees depending on what the customer's platform already supports.
