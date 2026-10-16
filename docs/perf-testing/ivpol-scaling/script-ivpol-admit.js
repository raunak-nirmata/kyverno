import { Kubernetes } from 'k6/x/kubernetes';
import { Rate } from 'k6/metrics';

// k6 workload for the ivpol arm: a Pod carrying IMAGES_PER_POD containers, each
// referencing a *distinct* signed image from the benchmark registry.
//
// Distinctness is the whole point. Kyverno fetches a manifest per image
// reference before it verifies anything, and #17313 changed that fetch from
// twenty-at-a-time to one-at-a-time. N containers sharing one image would
// collapse to a single fetch and measure nothing; N distinct images make the
// per-image cost visible, and the shape of the curve against N is the headline
// chart.
//
// The same N tags are reused across every Pod in a cell rather than drawn fresh
// per iteration. Cache-on cells need repetition to have anything to hit, and
// cache-off cells are unaffected: the manifest prefetch runs per request
// regardless of cache state, because ImageContext is built fresh per admission
// request and the verification cache is consulted later, inside the crypto path
// (pkg/image/verification/evaluator/policy.go:180 vs
// pkg/cel/libs/imageverify/impl.go:168).

const IMAGES_PER_POD = parseInt(__ENV.IMAGES_PER_POD || '1', 10);
const REGISTRY_HOST = __ENV.REGISTRY_HOST || 'registry.bench-registry.svc.cluster.local:5000';
const REPO = __ENV.REPO || 'bench/signed';
const NAMESPACE = __ENV.TEST_NAMESPACE || 'testns';

function containers() {
    const list = [];
    for (let i = 1; i <= IMAGES_PER_POD; i++) {
        list.push({
            name: `c${i}`,
            image: `${REGISTRY_HOST}/${REPO}:v${i}`,
            command: ['sh', '-c', 'sleep 30'],
            securityContext: {
                allowPrivilegeEscalation: false,
                runAsNonRoot: true,
                capabilities: { drop: ['ALL'] },
                seccompProfile: { type: 'RuntimeDefault' },
            },
        });
    }
    return list;
}

// Built once per VU rather than per iteration: the object is identical every
// time, and rebuilding it would charge the loader for work the server never
// sees. generateName keeps the names unique without rebuilding the spec.
const podSpec = {
    apiVersion: 'v1',
    kind: 'Pod',
    metadata: {
        generateName: 'ivpol-bench-',
        namespace: NAMESPACE,
    },
    spec: {
        securityContext: {
            runAsNonRoot: true,
            runAsUser: 1000,
            seccompProfile: { type: 'RuntimeDefault' },
        },
        containers: containers(),
        // Land on the KWOK fake node: these images are FROM scratch and cannot
        // run, and a real kubelet would try to pull them. KWOK fakes the
        // kubelet, so admission is exercised and nothing is ever pulled.
        affinity: {
            nodeAffinity: {
                requiredDuringSchedulingIgnoredDuringExecution: {
                    nodeSelectorTerms: [
                        {
                            matchExpressions: [
                                { key: 'type', operator: 'In', values: ['kwok'] },
                            ],
                        },
                    ],
                },
            },
        },
        tolerations: [
            { key: 'kwok.x-k8s.io/node', operator: 'Exists', effect: 'NoSchedule' },
        ],
    },
};

// Admission failures are a first-class metric, not an exception to let escape.
// A rejected or timed-out request still completes an iteration, and its duration
// is whatever the failure path cost - which is usually fast. Without this, a
// saturated cell reports a *better* latency than a healthy one, and the
// concurrency ramp has no stop condition.
const admissionFailed = new Rate('admission_failed');

export default function () {
    const kubernetes = new Kubernetes();
    try {
        kubernetes.create(podSpec);
        admissionFailed.add(false);
    } catch (e) {
        admissionFailed.add(true);
    }
}
