package dag

import "testing"

func TestTopologicalOrderRejectsCycle(t *testing.T) {
	graph := New()
	graph.AddNode("a")
	graph.AddNode("b")
	if err := graph.AddEdge("a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := graph.AddEdge("b", "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.TopologicalOrder(); err == nil {
		t.Fatal("expected cycle error")
	}
}
