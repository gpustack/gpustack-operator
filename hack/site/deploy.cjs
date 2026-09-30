// Deploy the uploaded snapshot using its published content commit as the build identity.
module.exports = async ({github, context, core}) => {
  const commit = process.env.PAGES_COMMIT;
  const artifact = process.env.PAGES_ARTIFACT_ID;
  if (!/^[0-9a-f]{40}$/.test(commit || "") || !/^\d+$/.test(artifact || "")) {
    throw new Error("Pages requires a full content commit and an uploaded artifact ID");
  }
  const repository = context.repo;
  const token = await core.getIDToken();
  core.setSecret(token);
  const {data: deployment} = await github.request("POST /repos/{owner}/{repo}/pages/deployments", {
    ...repository, artifact_id: Number(artifact), pages_build_version: commit,
    oidc_token: token, environment: "github-pages",
  });
  const id = deployment.id;
  if (!id || !deployment.page_url) throw new Error("Pages returned no deployment identity or URL");
  core.setOutput("page_url", deployment.page_url);
  core.info(`Deploying content ${commit}; deployment ${id}`);
  let pending = true;
  async function cancel() {
    if (pending) await github.request("POST /repos/{owner}/{repo}/pages/deployments/{pages_deployment_id}/cancel", {
      ...repository, pages_deployment_id: id,
    });
  }
  const interrupt = () => cancel().finally(() => process.exit(1));
  process.once("SIGINT", interrupt);
  process.once("SIGTERM", interrupt);
  const failures = new Set(["deployment_failed", "deployment_content_failed", "deployment_cancelled", "deployment_lost"]);
  const deadline = Date.now() + 600000;
  let errors = 0;
  try {
    while (Date.now() < deadline) {
      let status;
      try {
        const response = await github.request("GET /repos/{owner}/{repo}/pages/deployments/{pages_deployment_id}", {
          ...repository, pages_deployment_id: id,
        });
        status = response.data.status;
        errors = 0;
      } catch (error) {
        if (++errors > 10 || (error.status && error.status < 500 && error.status !== 404 && error.status !== 429)) throw error;
        core.warning("Pages status temporarily unavailable; retrying");
      }
      if (status === "succeed") {
        pending = false;
        return;
      }
      if (failures.has(status)) {
        pending = false;
        throw new Error(`Pages deployment ${id}: ${status}`);
      }
      await new Promise(resolve => setTimeout(resolve, 10000));
    }
    throw new Error(`Pages deployment ${id} timed out`);
  } finally {
    process.removeListener("SIGINT", interrupt);
    process.removeListener("SIGTERM", interrupt);
    await cancel();
  }
};
