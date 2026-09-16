package dag

import (
	"fmt"
	"sort"
)

type Graph struct {
	nodes map[string]struct{}
	edges map[string]map[string]struct{}
}

func New() *Graph {
	return &Graph{nodes: make(map[string]struct{}), edges: make(map[string]map[string]struct{})}
}

func (graph *Graph) AddNode(nodeID string) {
	graph.nodes[nodeID] = struct{}{}
}

func (graph *Graph) AddEdge(upstreamID, downstreamID string) error {
	if _, exists := graph.nodes[upstreamID]; !exists {
		return fmt.Errorf("unknown upstream node %q", upstreamID)
	}
	if _, exists := graph.nodes[downstreamID]; !exists {
		return fmt.Errorf("unknown downstream node %q", downstreamID)
	}
	if graph.edges[upstreamID] == nil {
		graph.edges[upstreamID] = make(map[string]struct{})
	}
	graph.edges[upstreamID][downstreamID] = struct{}{}
	return nil
}

func (graph *Graph) TopologicalOrder() ([]string, error) {
	inDegree := make(map[string]int, len(graph.nodes))
	for nodeID := range graph.nodes {
		inDegree[nodeID] = 0
	}
	for _, downstreamSet := range graph.edges {
		for downstreamID := range downstreamSet {
			inDegree[downstreamID]++
		}
	}

	ready := make([]string, 0)
	for nodeID, degree := range inDegree {
		if degree == 0 {
			ready = append(ready, nodeID)
		}
	}
	sort.Strings(ready)

	order := make([]string, 0, len(graph.nodes))
	for len(ready) > 0 {
		nodeID := ready[0]
		ready = ready[1:]
		order = append(order, nodeID)
		for downstreamID := range graph.edges[nodeID] {
			inDegree[downstreamID]--
			if inDegree[downstreamID] == 0 {
				ready = append(ready, downstreamID)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(graph.nodes) {
		return nil, fmt.Errorf("workflow dependency graph contains a cycle")
	}
	return order, nil
}
