import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { Root } from "./browser/Gate";
import { createBridgeClient, hasBridge } from "./client/adapters/bridge";
import { createHttpsClient } from "./client/adapters/https";
import { restoreMotion } from "./motion/effects";
import "./styles/index.css";

// The theme is already on <html> (public/theme-boot.js); a saved motion choice goes on <body>.
restoreMotion();

// The desktop shell's preload bridge is its only way to adad; without it this page is in a
// browser on adad's network listener, which it reaches over HTTPS. Only this module chooses.
const root = document.getElementById("root");
if (!root) throw new Error("ui: no #root element");
createRoot(root).render(
  <StrictMode>
    <Root client={hasBridge() ? createBridgeClient() : createHttpsClient()} />
  </StrictMode>,
);
