import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
// Poppins, bundled with the app (the site's security policy loads fonts from itself only).
import "@fontsource/poppins/300.css";
import "@fontsource/poppins/400.css";
import "@fontsource/poppins/500.css";
import "@fontsource/poppins/600.css";
import "@xyflow/react/dist/style.css";
import "./styles.css";
import { App } from "./App";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
