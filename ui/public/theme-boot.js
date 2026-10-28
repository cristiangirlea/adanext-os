// Runs as a blocking classic script in <head>, before any stylesheet, so the saved theme is on
// <html> before first paint and the CSP needs no inline script. Anything unknown means light.
(function () {
  var themes = ["light", "workshop"];
  var theme = "light";
  try {
    var saved = window.localStorage.getItem("ada.theme");
    if (themes.indexOf(saved) !== -1) theme = saved;
  } catch (e) {
    // Storage unavailable: keep the default.
  }
  document.documentElement.setAttribute("data-theme", theme);
})();
