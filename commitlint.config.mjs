// Conventional Commits; semantic-release derives versions from these.
export default {
  extends: ["@commitlint/config-conventional"],
  rules: {
    // Release commits embed the generated notes as their body.
    "body-max-line-length": [0],
    "footer-max-line-length": [0],
  },
};
