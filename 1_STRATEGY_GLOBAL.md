
## Why security teams resist Istio CNI on hardened clusters

### 1 — It's a privileged DaemonSet on every node

```
istio-cni DaemonSet
  → runs privileged
  → mounts /opt/cni/bin      (can replace CNI binaries)
  → mounts /etc/cni/net.d    (can modify CNI config)
  → mounts /proc             (can access all pod netns)
  → has CAP_NET_ADMIN on the NODE
```

From a security team's perspective:
> *"You want to install a third-party privileged binary that has write access to our CNI configuration and can enter any pod's network namespace on every node in our cluster?"*

That's a **very hard sell**.

---

### 2 — OpenShift specifically

OpenShift has its own CNI (OVN-Kubernetes) and is extremely locked down:

```
OpenShift security constraints:
  → SCCs (Security Context Constraints) replace PSS
  → privileged SCCs require explicit approval
  → OVN-Kubernetes CNI is tightly integrated with the platform
  → Red Hat support void if you modify CNI chain
  → FIPS compliance requirements may conflict
  → CNI binary directory is read-only in some configurations
```

Red Hat's official position is essentially:
> *"Use OpenShift Service Mesh (their Istio distribution) or don't use Istio at all"*

And even OpenShift Service Mesh requires significant security review in high-security environments.

---

### 3 — Other hardened environments that will push back

| Environment | Why they'll resist |
|---|---|
| OpenShift | SCCs, OVN-Kubernetes integration, Red Hat support |
| NSA/CISA hardened clusters | STIG requirements forbid unauthorized privileged DaemonSets |
| Financial sector clusters | Change management, audit requirements, third-party software approval |
| Air-gapped clusters | Can't pull Istio images, registry approval process |
| CIS Benchmark clusters | Privileged containers flagged automatically |

---

## The deeper problem you're facing

You're building an application that ships to customers with varying security postures:

```
Customer A: startup, GKE, relaxed security    → Istio CNI fine
Customer B: bank, OpenShift, strict security  → Istio CNI rejected
Customer C: government, air-gapped            → nothing external approved
```

**There is no single transparent redirection solution that works everywhere.**

---

## What enterprise ISVs actually do in practice

After all this technical exploration, here's what real software vendors shipping to high-security customers actually implement:

### Tier 1 — Application-level (works everywhere, no cluster permissions)
```
HTTP_PROXY + HTTPS_PROXY env vars
  + JAVA_TOOL_OPTIONS for JVM
  + git config
  + apt/yum proxy config
  + NetworkPolicy to fail-closed
```
> Covers ~90% of traffic. Requires zero cluster privileges. Accepted by every security team.

### Tier 2 — Platform-assisted (requires customer cooperation)
```
Customer's existing CNI egress gateway
  (Cilium EgressGateway, Calico Enterprise, OVN-Kubernetes EgressIP)
```
> Customer configures it using their own approved tools. You just document the requirement.

### Tier 3 — Full transparent (requires privileged install, case by case)
```
Istio CNI plugin
  → only offered to customers who explicitly approve it
  → goes through their change management process
  → not your default installation path
```

---

## Reframing your security requirement

The key question is: **what is the actual threat model?**

| Threat | Best mitigation |
|---|---|
| App accidentally calls internet | HTTP_PROXY + NetworkPolicy |
| Malicious code in app bypasses proxy | NetworkPolicy (fails closed) + admission control |
| Developer deliberately bypasses proxy | NetworkPolicy + audit logging |
| Zero-day in app exfiltrates data | NetworkPolicy + egress firewall at VPC level |

For most of these threats, **NetworkPolicy failing closed is actually sufficient** — because even if traffic isn't transparently redirected, it simply cannot reach the internet.

---

## Honest recommendation for your use case

```
Ship your application with:

1. HTTP_PROXY env vars          → transparent for most tools
2. NetworkPolicy egress block   → hard enforcement, works on every CNI
3. Document Tier 2/3 options    → for customers who need full transparency
                                   using their own approved mechanisms
```

And in your documentation:
```
"For full transparent traffic interception, your platform team can configure:
  - Cilium EgressGatewayPolicy (if using Cilium)
  - OpenShift EgressIP (if using OpenShift)
  - Istio CNI plugin (if Istio is approved in your environment)
 
 These are optional enhancements. The default installation uses
 NetworkPolicy to enforce egress restrictions without privileged components."
```

This way **you never ask for privileged access** — you let the customer's platform team use tools they already own and trust.
