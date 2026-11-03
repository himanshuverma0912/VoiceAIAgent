// Runs before the page draws (a file, not inline: the site's security policy
// allows scripts from itself only), so a dark-mode user never sees a white flash.
// The saved choice wins; otherwise the computer's light/dark setting.
(function () {
  var saved = null;
  try {
    saved = localStorage.getItem("flow-studio.theme");
  } catch (e) {}
  var dark = saved ? saved === "dark" : window.matchMedia && matchMedia("(prefers-color-scheme: dark)").matches;
  document.documentElement.dataset.theme = dark ? "dark" : "light";
})();
