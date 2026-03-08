CLUSTER_NAME  := kubernetes-proxy
ISTIO_VERSION := 1.23.0

# Force bash so Unix commands work on Windows (Git Bash / WSL).
SHELL := bash

.PHONY: all all-istio-cni all-custom-cni \
        cluster-create cluster-delete \
        istio-install istio-patch-webhook \
        build build-fuse load push push-fuse seccomp \
        build-cni load-cni push-cni cni-install cni-uninstall \
        deploy restart \
        redeploy redeploy-istio-cni-vfs redeploy-custom-cni-vfs \
        redeploy-istio-cni-fuse redeploy-custom-cni-fuse \
        undeploy undeploy-istio-cni undeploy-custom-cni \
        logs-proxy logs-sidecar logs-app \
        test clean

# --------------------------------------------------------------------------- #
# Cluster                                                                      #
# --------------------------------------------------------------------------- #

## Create the kind cluster.
cluster-create:
	kind create cluster --config k8s/kind-config.yaml
	kubectl cluster-info --context kind-$(CLUSTER_NAME)

## Delete the kind cluster.
cluster-delete:
	kind delete cluster --name $(CLUSTER_NAME)

# --------------------------------------------------------------------------- #
# Istio                                                                        #
# --------------------------------------------------------------------------- #

## Add the Istio Helm repository.
helm-add-istio:
	helm repo add istio https://istio-release.storage.googleapis.com/charts
	helm repo update

## Install Istio base CRDs.
istio-base:
	kubectl create namespace istio-system --dry-run=client -o yaml | kubectl apply -f -
	helm upgrade --install istio-base istio/base \
		--namespace istio-system \
		--version $(ISTIO_VERSION) \
		--wait

## Install istiod (control plane).
istio-istiod: istio-base
	helm upgrade --install istiod istio/istiod \
		--namespace istio-system \
		--version $(ISTIO_VERSION) \
		--values k8s/istio/values-istiod.yaml \
		--wait

## Install Istio CNI (chained with kindnet).
istio-cni: istio-istiod
	helm upgrade --install istio-cni istio/cni \
		--namespace kube-system \
		--version $(ISTIO_VERSION) \
		--values k8s/istio/values-cni.yaml \
		--wait

## Patch the sidecar-injector webhook to decouple it from CNI (see patch-webhook.yaml).
istio-patch-webhook:
	kubectl patch mutatingwebhookconfiguration istio-sidecar-injector \
		--patch-file k8s/istio/patch-webhook.yaml

## Full Istio install + webhook patch.
istio-install: istio-cni istio-patch-webhook

# --------------------------------------------------------------------------- #
# Images                                                                       #
# --------------------------------------------------------------------------- #

## Build app images (proxy, sidecar, app) — vfs storage driver (restricted PSA).
build:
	docker build -t proxy:latest   ./go/proxy
	docker build -t sidecar:latest \
		-f go/sidecar/docker/vfs/Dockerfile \
		./go/sidecar
	docker build -t app:latest     ./go/app

## Build app images with the fuse/overlay storage driver (baseline PSA).
## Requires a /dev/fuse hostPath volume in the pod spec.
build-fuse:
	docker build -t proxy:latest   ./go/proxy
	docker build -t sidecar:latest \
		-f go/sidecar/docker/fuse/Dockerfile \
		./go/sidecar
	docker build -t app:latest     ./go/app

## Deploy the Localhost seccomp profile to all kind nodes (required for Podman).
## The profile is read from k8s/seccomp/podman.json.
## Must be re-run after every cluster restart — profiles do not persist.
seccomp:
	@for node in $$(kind get nodes --name $(CLUSTER_NAME)); do \
		docker exec "$$node" mkdir -p /var/lib/kubelet/seccomp; \
		docker cp k8s/seccomp/podman.json "$$node:/var/lib/kubelet/seccomp/podman.json"; \
		echo "deployed seccomp profile to $$node"; \
	done

## Load app images into the kind cluster (no registry needed).
load:
	kind load docker-image proxy:latest   --name $(CLUSTER_NAME)
	kind load docker-image sidecar:latest --name $(CLUSTER_NAME)
	kind load docker-image app:latest     --name $(CLUSTER_NAME)

## Build + load app images in one step (vfs).
push: build load

## Build + load app images with fuse/overlay storage driver.
push-fuse: build-fuse load

# --------------------------------------------------------------------------- #
# Custom CNI plugin                                                            #
# --------------------------------------------------------------------------- #

## Build the custom CNI plugin image.
build-cni:
	docker build -t cni-plugin:latest ./go/cni-plugin

## Load the CNI plugin image into the kind cluster.
load-cni:
	kind load docker-image cni-plugin:latest --name $(CLUSTER_NAME)

## Build + load CNI plugin image.
push-cni: build-cni load-cni

## Install the custom CNI plugin DaemonSet (alternative to Istio CNI).
## The DaemonSet patches the kindnet conflist to add egress-proxy as a
## chained plugin. No "istio-proxy" container name required.
cni-install: push-cni
	kubectl apply -f k8s/cni-plugin/
	kubectl rollout status daemonset/egress-proxy-cni -n kube-system --timeout=60s

## Remove the custom CNI plugin DaemonSet.
## The installer restores the original conflist on graceful shutdown.
cni-uninstall:
	kubectl delete -f k8s/cni-plugin/ --ignore-not-found

# --------------------------------------------------------------------------- #
# Kubernetes resources                                                         #
# --------------------------------------------------------------------------- #

## Apply all manifests.
deploy:
	kubectl apply -f k8s/namespaces.yaml
	kubectl apply -f k8s/proxy/
	kubectl apply -f k8s/app/
	kubectl rollout status deployment/proxy -n proxy --timeout=60s
	kubectl rollout status deployment/app   -n app   --timeout=60s

## Restart all deployments (pick up new images already loaded into kind).
restart:
	kubectl rollout restart deployment/proxy -n proxy
	kubectl rollout restart deployment/app   -n app
	kubectl rollout status  deployment/proxy -n proxy --timeout=60s
	kubectl rollout status  deployment/app   -n app   --timeout=60s

## Rebuild app images (vfs sidecar), reload into kind, then restart (Istio CNI mode).
redeploy-istio-cni-vfs: push restart

## Rebuild app images (fuse sidecar), reload into kind, then restart (Istio CNI mode).
redeploy-istio-cni-fuse: push-fuse restart

## Rebuild app + CNI plugin images (vfs sidecar), reload into kind, then restart (custom CNI mode).
redeploy-custom-cni-vfs: push push-cni restart

## Rebuild app + CNI plugin images (fuse sidecar), reload into kind, then restart (custom CNI mode).
redeploy-custom-cni-fuse: push-fuse push-cni restart

## Alias for redeploy-istio-cni-vfs.
redeploy: redeploy-istio-cni-vfs

## Delete all manifests (keeps cluster and Istio).
undeploy-istio-cni:
	kubectl delete -f k8s/app/    --ignore-not-found
	kubectl delete -f k8s/proxy/  --ignore-not-found
	kubectl delete -f k8s/namespaces.yaml --ignore-not-found

## Remove custom CNI plugin then delete all manifests.
undeploy-custom-cni: cni-uninstall undeploy-istio-cni

## Delete all manifests (defaults to Istio CNI mode).
undeploy: undeploy-istio-cni

# --------------------------------------------------------------------------- #
# Observability                                                                #
# --------------------------------------------------------------------------- #

logs-proxy:
	kubectl logs -n proxy -l app=proxy --all-containers -f

logs-sidecar:
	kubectl logs -n app -l app=app -c istio-proxy -f

logs-app:
	kubectl logs -n app -l app=app -c app -f

## Ad-hoc test: exec into the running app pod and fetch a URL through the proxy.
## The app container runs as UID 10000, so iptables intercepts the request and
## routes it through the sidecar → central proxy, exactly like the app does.
test:
	kubectl exec -n app deploy/app -c app -- wget -qO- http://httpbin.org/get

# --------------------------------------------------------------------------- #
# Utility                                                                      #
# --------------------------------------------------------------------------- #

## Full setup using Istio CNI.
## NOTE: Istio CNI requires the sidecar container to be named "istio-proxy".
##       Use all-custom-cni to avoid that constraint.
all-istio-cni: cluster-create helm-add-istio istio-install push deploy

## Shortcut for all-istio-cni.
all: all-istio-cni

## Full setup using the custom CNI plugin (no Istio required, no naming constraints).
all-custom-cni: cluster-create push cni-install deploy

## Tear everything down.
clean: cluster-delete
