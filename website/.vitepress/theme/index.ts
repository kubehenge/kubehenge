import type { Theme } from "vitepress";
import DefaultTheme from "vitepress/theme";
import { h } from "vue";

import HengeHome from "./HengeHome.vue";
import Logo from "./Logo.vue";

import "./style.css";

export default {
  extends: DefaultTheme,
  Layout: () => h(DefaultTheme.Layout, null, { "nav-bar-title-before": () => h(Logo) }),
  enhanceApp({ app }) {
    app.component("HengeHome", HengeHome);
  },
} satisfies Theme;
