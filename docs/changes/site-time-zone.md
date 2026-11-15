### Added

- A site **Time zone** in Setup and Administration, and `timeZone` in the chart values. Schedules that name none use it, in the wizard and through kubectl. Left empty, schedules run in UTC and the wizard starts from the browser's time zone.
