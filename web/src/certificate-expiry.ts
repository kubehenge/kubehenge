const msPerDay = 86_400_000;

/** Administrators are warned this many days before a certificate runs out. */
export const certificateWarningDays = 30;

/** daysLeft counts the whole days from now to an instant, negative once it has passed. */
export const daysLeft = (instant: Date, now = Date.now()): number => Math.floor((instant.getTime() - now) / msPerDay);

/** runsOutSoon tells whether an instant is less than certificateWarningDays away, or has passed. */
export const runsOutSoon = (instant: Date, now = Date.now()): boolean =>
  instant.getTime() - now < certificateWarningDays * msPerDay;
