import { Kubernetes } from 'k6/x/kubernetes';

// Same workload as ../../../scripts/k6/script.js, but the pod carries a
// restricted-PSS-compliant securityContext so all 17 policies PASS and the pod
// is ADMITTED (then scheduled onto the KWOK fake node via the affinity below,
// and persisted to etcd). Use this to compare the admit path against the deny
// path that the stock script.js produces against restricted policies.
const podSpec = {
    apiVersion: "v1",
    kind: "Pod",
    metadata: {
        generateName: "busybox",
        namespace: "testns"
    },
    spec: {
        securityContext: {
            runAsNonRoot: true,
            runAsUser: 1000,
            seccompProfile: { type: "RuntimeDefault" }
        },
        containers: [
            {
                name: "busybox",
                image: "busybox",
                command: ["sh", "-c", "sleep 30"],
                securityContext: {
                    allowPrivilegeEscalation: false,
                    runAsNonRoot: true,
                    capabilities: { drop: ["ALL"] },
                    seccompProfile: { type: "RuntimeDefault" }
                }
            }
        ],
        affinity: {
            nodeAffinity: {
                requiredDuringSchedulingIgnoredDuringExecution: {
                    nodeSelectorTerms: [
                        {
                            matchExpressions: [
                                { key: "type", operator: "In", values: ["kwok"] }
                            ]
                        }
                    ]
                }
            }
        },
        tolerations: [
            { key: "kwok.x-k8s.io/node", operator: "Exists", effect: "NoSchedule" }
        ]
    }
}

export default function () {
    const kubernetes = new Kubernetes();
    kubernetes.create(podSpec)
}
