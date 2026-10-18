import type { Theme } from "vitepress";
import DefaultTheme from "vitepress/theme";

import HengeHome from "./HengeHome.vue";

import "./style.css";

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component("HengeHome", HengeHome);
  },
} satisfies Theme;
