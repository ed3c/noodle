#!/usr/bin/env python3
"""Project a Go review graph into the unchanged UML Viewer EDN surface."""

import argparse
import json
from pathlib import Path


def edn(value):
    if value is None:
        return "nil"
    if isinstance(value, str):
        return json.dumps(value, ensure_ascii=False)
    if isinstance(value, dict):
        return "{" + " ".join(":" + key + " " + edn(item) for key, item in value.items()) + "}"
    if isinstance(value, list):
        return "[" + " ".join(map(edn, value)) + "]"
    raise ValueError(f"unsupported projection value: {type(value).__name__}")


def diagram(graph):
    if graph.get("schema") != 1 or graph.get("analyzer") != "noodle-go-types-v1":
        raise ValueError("unsupported source graph")
    nodes = {node["id"]: node for node in graph["nodes"]}
    if len(nodes) != len(graph["nodes"]):
        raise ValueError("duplicate source node")
    containers = {}
    for edge in graph["edges"]:
        if edge["from"] not in nodes or edge["to"] not in nodes:
            raise ValueError("edge references missing source node")
        if edge["kind"] == "contains":
            containers[edge["to"]] = edge["from"]
    packages = {}
    classes = {}
    for node in graph["nodes"]:
        if node["kind"] != "file":
            continue
        package = packages.setdefault(node["package"], {"id": containers[node["id"]],
                                     "label": node["package"], "classes": []})
        item = {"id": node["id"], "name": node["name"], "stereotype": "Go file",
                "ns": node["package"] + "/" + node["name"], "lang": "go", "ops": []}
        # Do not expose live repository paths to the desktop source opener.
        classes[node["id"]] = item
        package["classes"].append(item)
    foreign = []
    for node in graph["nodes"]:
        if node["kind"] in ("type", "function"):
            parent = containers[node["id"]]
            classes[parent]["ops"].append({"name": node["name"]})
        elif node["kind"] == "external_function":
            foreign.append({"id": node["id"], "name": node["name"],
                            "stereotype": "external target; body not analyzed", "ops": []})
    edges, seen = [], set()
    for edge in graph["edges"]:
        if edge["kind"] != "static_call":
            continue
        source = containers[edge["from"]]
        target = containers.get(edge["to"], edge["to"])
        if source == target or (source, target) in seen:
            continue
        seen.add((source, target))
        edges.append({"from": source, "to": target, "kind": "dependency",
                      "label": "static call (file projection)"})
    return {"title": "Go source projection — no runtime or quality PASS; "
                     + str(len(graph["unknowns"])) + " unresolved call sites",
            "packages": list(packages.values()), "foreign": foreign, "edges": edges}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("graph", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    source = json.loads(args.graph.read_text())
    text = edn(diagram(source)) + "\n"
    with args.output.open("x") as output:
        output.write(text)


if __name__ == "__main__":
    main()
