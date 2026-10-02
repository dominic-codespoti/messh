package main

import "fmt"

func llmConfigAgent(harness, agent string) (string, error) {
	switch harness {
	case "omp", "pi":
		if agent == "" {
			agent = harness
		}
	case "openai":
		if agent == "" {
			return "", fmt.Errorf("--agent NAME is required for generic config (the agent whose token the client will use)")
		}
	default:
		return "", fmt.Errorf("--for must be omp, pi or openai, not %q", harness)
	}
	return agent, nil
}
