package helmx

import (
	"fmt"

	helmv3 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/helm/v3"
	networkingv1 "github.com/pulumi/pulumi-kubernetes/sdk/v4/go/kubernetes/networking/v1"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumix"
)

func ingress(chart *helmv3.Chart, fqn, namespace string) pulumix.Output[*networkingv1.Ingress] {
	i := chart.GetResource("networking.k8s.io/v1/Ingress", fqn, namespace)

	in := i.ApplyT(func(i interface{}) *networkingv1.Ingress {
		return i.(*networkingv1.Ingress)
	})

	b, err := pulumix.ConvertTyped[*networkingv1.Ingress](in)
	if err != nil {
		panic(err)
	}

	return b
}

func IngressIP(chart *helmv3.Chart, fqn, namespace string) pulumix.Output[string] {
	ingress := ingress(chart, fqn, namespace)

	frontendIP := pulumix.ApplyErr(ingress, func(r *networkingv1.Ingress) (pulumix.Output[string], error) {
		status := r.Status
		loadBalancer := status.LoadBalancer()
		ingress := loadBalancer.Ingress()

		lbiao := ingress.ToIngressLoadBalancerIngressArrayOutput()

		ip := pulumix.ApplyErr(lbiao, func(vs []networkingv1.IngressLoadBalancerIngress) (string, error) {
			index := 0
			if len(vs) <= index {
				return "", fmt.Errorf("index out of range")
			}
			return *vs[index].Ip, nil
		})

		return ip, nil
	})

	fIP := pulumix.Flatten[string](frontendIP)

	return fIP
}
