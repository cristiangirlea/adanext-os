import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { createBridgeClient } from "./client/adapters/bridge";
import { restoreMotion } from "./motion/effects";
import "./styles/index.css";

// The theme is already on <html> (public/theme-boot.js); a saved motion choice goes on <body>.
restoreMotion();

// The desktop shell loads this page; the preload bridge is its only way to adad. The browser
// shell (M4b) will choose the HTTPS adapter here instead.
const root = document.getElementById("root");
if (!root) throw new Error("ui: no #root element");
createRoot(root).render(
  <StrictMode>
    <App client={createBridgeClient()} />
  </StrictMode>,
);
