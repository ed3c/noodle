import { createFileRoute } from "@tanstack/react-router";
import { LoopGraphPanel } from "~/components/LoopGraph";

export const Route = createFileRoute("/loops")({ component: LoopGraphPanel });
