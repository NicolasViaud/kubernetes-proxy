CLUSTER_NAME  := kubernetes-proxy
ISTIO_VERSION := 1.23.0

# Force bash so Unix commands work on Windows (Git Bash / WSL).
SHELL := bash

.PHONY: all all-istio-cni all-custom-cni \
        cluster-create cluster-delete \
        istio-install istio-patch-webhook \
        build load push \
        build-cni load-cni push-cni cni-install cni-uninstall \
        deploy restart redeploy undeploy \
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

## Build app images (proxy, sidecar, app).
build:
	docker build -t proxy:latest   ./go/proxy
	docker build -t sidecar:latest ./go/sidecar
	docker build -t app:latest     ./go/app

## Load app images into the kind cluster (no registry needed).
load:
	kind load docker-image proxy:latest   --name $(CLUSTER_NAME)
	kind load docker-image sidecar:latest --name $(CLUSTER_NAME)
	kind load docker-image app:latest     --name $(CLUSTER_NAME)

## Build + load app images in one step.
push: build load

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

## Rebuild images, reload into kind, then restart.
redeploy: push restart

## Delete all manifests (keeps cluster and Istio).
undeploy:
	kubectl delete -f k8s/app/    --ignore-not-found
	kubectl delete -f k8s/proxy/  --ignore-not-found
	kubectl delete -f k8s/namespaces.yaml --ignore-not-found

# --------------------------------------------------------------------------- #
# Observability                                                                #
# --------------------------------------------------------------------------- #

logs-proxy:
	kubectl logs -n proxy -l app=proxy --all-containers -f

logs-sidecar:
	kubectl logs -n app -l app=app -c istio-proxy -f

logs-app:
	kubectl logs -n app -l app=app -c app -f

## Run a one-shot curl pod in the app namespace to test interception manually.
test:
	kubectl run curl-test \
		--image=curlimages/curl:latest \
		--namespace=app \
		--restart=Never \
		--rm -it \
		--annotations='traffic.sidecar.istio.io/includeOutboundIPRanges=*' \
		--annotations='traffic.sidecar.istio.io/includeInboundPorts=' \
		--annotations='sidecar.istio.io/proxyUID=1337' \
		--annotations='sidecar.istio.io/proxyGID=1337' \
		--overrides='{"spec":{"containers":[{"name":"curl","image":"curlimages/curl:latest","command":["curl","-v","http://httpbin.org/get"],"securityContext":{"runAsUser":10001,"runAsNonRoot":true,"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}},{"name":"istio-proxy","image":"sidecar:latest","imagePullPolicy":"Never","env":[{"name":"LISTEN_ADDR","value":":15001"},{"name":"PROXY_ADDR","value":"proxy-service.proxy.svc.cluster.local:8080"}],"securityContext":{"runAsUser":1337,"runAsGroup":1337,"runAsNonRoot":true,"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}],"securityContext":{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}}}' \
		-- curl -v http://httpbin.org/get

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
