const time = new Date().toISOString();
log.info('cron tick at ' + time);
return { time };
