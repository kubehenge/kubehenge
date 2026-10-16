// Adds a Kubernetes entry to Proxmox VE's interface that leads to Kubehenge's appliance: a button
// beside Create VM and Create CT, and an item under Datacenter. Loaded right after
// pvemanagerlib.js, so these overrides are in place before the workspace is built. The appliance
// opens in a tab of its own: it refuses to be framed, and stays a separate origin from Proxmox VE.
// Not strict mode: ExtJS's callParent finds the overridden method through the caller chain, which
// strict functions do not expose, and the workspace would fail to render.
(function kubehengeEntry() {
  // Set by the installer.
  const applianceURL = "@APPLIANCE_URL@";

  function open() {
    window.open(applianceURL, "_blank", "noopener,noreferrer");
  }

  // Proxmox VE's interface classes are no API: if a release renames one, or adding the entry to
  // it fails, the entry stays out and Proxmox VE builds its page as it would without it.
  function extend(name, add) {
    if (!Ext.ClassManager.get(name)) {
      console.warn(`Kubehenge: ${name} not found; the Kubernetes entry is left out`);
      return;
    }
    Ext.define(`Kubehenge.override.${name}`, {
      override: name,

      initComponent(...args) {
        this.callParent(args);
        try {
          add.call(this);
        } catch (err) {
          console.warn(`Kubehenge: the Kubernetes entry is left out of ${name}`, err);
        }
      },
    });
  }

  extend("PVE.StdWorkspace", function addButton() {
    const userinfo = this.down("#userinfo");
    const bar = userinfo && userinfo.ownerCt;
    if (!bar) {
      return;
    }
    bar.insert(bar.items.indexOf(userinfo), {
      xtype: "button",
      itemId: "kubehenge",
      pack: "end",
      margin: "0 5 0 0",
      baseCls: "x-btn",
      iconCls: "fa fa-ship",
      text: gettext("Kubernetes"),
      tooltip: applianceURL,
      handler: open,
    });
  });

  extend("PVE.dc.Config", function addDatacenterItem() {
    this.insertNodes([
      {
        xtype: "panel",
        itemId: "kubehenge",
        title: gettext("Kubernetes"),
        iconCls: "fa fa-ship",
        bodyPadding: 10,
        items: [
          {
            xtype: "component",
            html: Ext.String.htmlEncode(
              gettext(
                "Kubernetes clusters on these nodes are managed by Kubehenge, in its own interface. Sign in there with your Proxmox VE account.",
              ),
            ),
            margin: "0 0 10 0",
          },
          {
            xtype: "button",
            iconCls: "fa fa-external-link",
            text: gettext("Open Kubehenge"),
            handler: open,
          },
          {
            xtype: "component",
            html: Ext.String.htmlEncode(applianceURL),
            margin: "10 0 0 0",
          },
        ],
      },
    ]);
  });
})();
