import { defineConfig } from "vitepress";

const repository = "https://github.com/kubehenge/kubehenge";

export default defineConfig({
  title: "Kubehenge",
  description: "Kubernetes for Proxmox VE",
  lang: "en-GB",
  cleanUrls: true,
  lastUpdated: true,
  head: [["link", { rel: "icon", type: "image/svg+xml", href: "/logo.svg" }]],
  sitemap: { hostname: "https://kubehenge.dev" },
  themeConfig: {
    logo: { src: "/logo.svg", alt: "" },
    nav: [
      { text: "Docs", link: "/docs/install" },
      { text: "Changelog", link: "/docs/changelog" },
    ],
    sidebar: [
      {
        text: "Using Kubehenge",
        items: [
          { text: "Installing", link: "/docs/install" },
          { text: "Clusters", link: "/docs/clusters" },
          { text: "Upgrading", link: "/docs/upgrading" },
          { text: "Uninstalling", link: "/docs/uninstalling" },
          { text: "Troubleshooting", link: "/docs/troubleshooting" },
          { text: "VM templates", link: "/docs/templates" },
        ],
      },
      {
        text: "How it is made",
        items: [
          { text: "Architecture", link: "/docs/architecture" },
          { text: "Testing", link: "/docs/testing" },
          { text: "Releasing", link: "/docs/releasing" },
          { text: "Roadmap", link: "/docs/roadmap" },
          { text: "Changelog", link: "/docs/changelog" },
        ],
      },
    ],
    socialLinks: [{ icon: "github", link: repository }],
    editLink: { pattern: `${repository}/edit/main/:path`, text: "Change this page on GitHub" },
    search: { provider: "local" },
    footer: {
      message:
        "Apache-2.0. Not affiliated with or endorsed by Proxmox Server Solutions GmbH; Proxmox is its registered trademark.",
    },
  },
});
