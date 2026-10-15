import { createFileRoute } from "@tanstack/react-router";
import { LoopViews } from "~/components/LoopViews";

export const Route = createFileRoute("/loops")({ component: LoopViews });
