package pipeline

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	yaml "go.yaml.in/yaml/v3"
)

const maxConfigDepth = 64
const maxConfigNodes = 131072
const maxExpansionBytes = 8 << 20
const maxAliasBytes = 2 << 20

// CFG-4: inspect syntax nodes before conversion, alias expansion or schema walking.
// JSON is a YAML subset and takes the same limits. Each alias dereference
// counts its target subtree again, bounding expanded values as well as syntax.
func checkDecodeLimits(data []byte, subject string, expansion ...bool) error {
	d := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := d.Decode(&root); err != nil {
		if strings.Contains(err.Error(), "exceeded max depth") {
			return &PathError{"$", fmt.Errorf("%s configuration exceeds nesting limit", subject)}
		}
		return &PathError{"$", yamlError(err)}
	}
	count, projected := 0, 0
	var expandedBytes, aliasBytes int64
	var walk func(*yaml.Node, int, int, bool, bool, bool) error
	walk = func(n *yaml.Node, depth, pathBytes int, key, atomic, aliased bool) error {
		count++
		if depth > maxConfigDepth {
			return &PathError{"$", fmt.Errorf("%s configuration exceeds nesting limit", subject)}
		}
		if count > maxConfigNodes {
			return &PathError{"$", fmt.Errorf("%s configuration exceeds node count limit", subject)}
		}
		if n.Kind == yaml.ScalarNode {
			// CFG-4: conversion escapes control/HTML characters in keys and values.
			// Reserve their encoded size without materializing quoted copies. The
			// separate alias cap bounds copies amplified from a small source.
			size := jsonStringSize(n.Value)
			expandedBytes += size
			if aliased {
				aliasBytes += size
			}
			if len(expansion) > 0 && expansion[0] && !key && !atomic {
				// Both the JSON pointer and source path repeat ancestor keys per leaf.
				// Refuse this lower bound before conversion allocates full decoded trees.
				projected += 2 * pathBytes
				if projected > maxExpansionBytes {
					return ErrExpansionBudget
				}
			}
			if expandedBytes > maxExpansionBytes || aliasBytes > maxAliasBytes {
				return &PathError{"$", fmt.Errorf("%s configuration exceeds expanded size limit", subject)}
			}
		}
		if n.Kind == yaml.AliasNode {
			// The target occupies the alias's level. Cycles terminate at the depth cap.
			return walk(n.Alias, depth, pathBytes, key, atomic, true)
		}
		for i, c := range n.Content {
			childBytes, isKey, childAtomic := pathBytes, false, atomic
			switch n.Kind {
			case yaml.MappingNode:
				isKey = i%2 == 0
				if !isKey {
					field := n.Content[i-1].Value
					childBytes += len(field) + 1
					target := c
					if target.Kind == yaml.AliasNode {
						target = target.Alias
					}
					// Opaque arrays have one origin for the entire array. Only schema
					// stage/image lists have per-element provenance.
					if target.Kind == yaml.SequenceNode && field != "stages" && field != "images" {
						childAtomic = true
					}
				}
			case yaml.SequenceNode:
				childBytes += 8
			}
			if err := walk(c, depth+1, childBytes, isKey, childAtomic, aliased); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(&root, 0, 0, false, false, false); err != nil {
		return err
	}
	if err := d.Decode(&root); err != io.EOF {
		return &PathError{"$", fmt.Errorf("decode YAML: expected exactly one YAML document")}
	}
	return nil
}
