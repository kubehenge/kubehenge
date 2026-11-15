### Added

- A site **Time zone** in Setup and Administration, and `timeZone` in the chart values. Schedules and upgrade windows that name none use it, in the wizard and through kubectl.
- Left empty or set to an unknown zone, the site time zone leaves schedules in UTC and the wizard in the browser's time zone.
