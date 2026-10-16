export interface LayoutNode {
  id: string;
  repository_id: string;
}
export interface PositionedNode extends LayoutNode {
  x: number;
  y: number;
  width: number;
  height: number;
  rank: number;
}
export function layoutGraph(input: {
  nodes: LayoutNode[];
  edges: { from: string; to: string }[];
}): {
  nodes: PositionedNode[];
  bands: { repository_id: string; x: number; y: number; width: number; height: number }[];
  width: number;
  height: number;
};
