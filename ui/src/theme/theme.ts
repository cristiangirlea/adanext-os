// The theme is a per-device preference kept in localStorage, never an adad setting. The boot
// script (public/theme-boot.js) applies the saved choice before first paint and must list the
// same themes and key; a test holds the two together.
export const themes = ["light", "workshop"] as const;
export type Theme = (typeof themes)[number];
export const themeKey = "ada.theme";

export const themeNames: Record<Theme, string> = {
  light: "Light slate",
  workshop: "Workshop",
};

function isTheme(value: unknown): value is Theme {
  return typeof value === "string" && (themes as readonly string[]).includes(value);
}

export function currentTheme(): Theme {
  const value = document.documentElement.dataset.theme;
  return isTheme(value) ? value : "light";
}

export function setTheme(theme: Theme): void {
  document.documentElement.dataset.theme = theme;
  try {
    localStorage.setItem(themeKey, theme);
  } catch {
    // Storage refused (a private window): the theme still applies for this page.
  }
}
