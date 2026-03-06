CLUSTER_NAME  := kubernetes-proxy
ISTIO_VERSION := 1.23.0
IMAGES        := proxy sidecar app

.PHONY: all cluster-create cluster-delete \
        istio-install istio-patch-webhook \
        build load deploy \
        logs-proxy logs-sidecar logs-app \
        test clean

# --------------------------------------------------------------------------- #
# Cluster                                                                      #
# --------------------------------------------------------------------------- #

## Create the kind cluster.
cluster-create:
	mkdir -p /tmp/k8s-proxy-cni-bin /tmp/k8s-proxy-cni-conf
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

## Build all Docker images.
build:
	docker build -t proxy:latest   ./proxy
	docker build -t sidecar:latest ./sidecar
	docker build -t app:latest     ./app

## Load images into the kind cluster (no registry needed).
load:
	kind load docker-image proxy:latest   --name $(CLUSTER_NAME)
	kind load docker-image sidecar:latest --name $(CLUSTER_NAME)
	kind load docker-image app:latest     --name $(CLUSTER_NAME)

## Build + load in one step.
push: build load

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
	kubectl logs -n app -l app=app -c sidecar -f

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
		--overrides='{"spec":{"containers":[{"name":"curl","image":"curlimages/curl:latest","command":["curl","-v","http://httpbin.org/get"],"securityContext":{"runAsUser":10001,"runAsNonRoot":true,"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}],"securityContext":{"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}}}}' \
		-- curl -v http://httpbin.org/get

# --------------------------------------------------------------------------- #
# Utility                                                                      #
# --------------------------------------------------------------------------- #

## Full local setup from scratch.
all: cluster-create helm-add-istio istio-install push deploy

## Tear everything down.
clean: cluster-delete
