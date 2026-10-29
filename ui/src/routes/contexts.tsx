import { createFileRoute } from "@tanstack/react-router";
import { ContextControls } from "~/components/ContextControls";
export const Route = createFileRoute("/contexts")({ component: ContextControls });
