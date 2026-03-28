log.info(`Trying`);

try {
  const clientId = env.get("GMAIL_CLIENT_ID");
  const scope = params.get("scope");

  // Step 1: Request device and user codes
  const deviceRes = await http.post("https://oauth2.googleapis.com/device/code", {
    body: { client_id: clientId, scope },
  });

  if (deviceRes.status !== 200) {
    log.error("Device code request failed", { status: deviceRes.status, body: deviceRes.body });
    throw new Error(`Device code error: ${deviceRes.status}`);
  }

  const { device_code, user_code, verification_url, expires_in, interval = 5 } = deviceRes.body;

  log.info("=================================================");
  log.info(`Visit: ${verification_url}`);
  log.info(`Enter code: ${user_code}`);
  log.info("=================================================");

  // Step 2: Poll until the user approves or the code expires
  const deadline = Date.now() + expires_in * 1000;
  let pollInterval = interval * 1000;

  while (Date.now() < deadline) {
    await new Promise(r => setTimeout(r, pollInterval));

    const tokenRes = await http.post("https://oauth2.googleapis.com/token", {
      body: {
        client_id: clientId,
        device_code,
        grant_type: "urn:ietf:params:oauth:grant-type:device_code",
      },
    });

    if (tokenRes.status === 200) {
      const { refresh_token, access_token, expires_in: tokenExpiry } = tokenRes.body;
      log.info("OAuth flow complete");
      log.info(`GMAIL_REFRESH_TOKEN=${refresh_token}`);
      return { refresh_token, access_token, expires_in: tokenExpiry };
    }

    const error = tokenRes.body?.error;

    if (error === "authorization_pending") {
      log.info("Waiting for user approval...");
      continue;
    }

    if (error === "slow_down") {
      pollInterval += 5000;
      log.info(`Slowing down polling to ${pollInterval / 1000}s`);
      continue;
    }

    // access_denied, expired_token, or unexpected error
    log.error("Token poll failed", { error, body: tokenRes.body });
    throw new Error(`OAuth error: ${error}`);
  }

  throw new Error("Device code expired before user approved");
} catch(e) {
  log.info(e.message);
}
log.info("done");
