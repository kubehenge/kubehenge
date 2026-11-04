// The interface's and the website's CSS: the standard config, a strict order for every rule's
// contents and properties, and logical properties throughout, as the pages are laid out for any
// writing direction.
/** @type {import("stylelint").Config} */
const config = {
  extends: ["stylelint-config-standard", "stylelint-config-clean-order/error"],
  plugins: ["stylelint-plugin-logical-css"],
  reportDescriptionlessDisables: true,
  reportInvalidScopeDisables: true,
  reportNeedlessDisables: true,
  reportUnscopedDisables: true,
  rules: {
    "logical-css/require-logical-keywords": true,
    // Safari has no overflow-inline or overflow-block.
    "logical-css/require-logical-properties": [true, { ignore: ["overflow-x", "overflow-y"] }],
    "logical-css/require-logical-units": true,
  },
};

export default config;
