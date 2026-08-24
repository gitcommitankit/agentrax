# tflint configuration for the infra/ directory.
# Enables the official Terraform plugin for provider-level schema validation.
# Run: tflint --chdir=infra/environments/dev

plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

# Enforce consistent code style.
rule "terraform_naming_convention" {
  enabled = true
}

rule "terraform_required_version" {
  enabled = true
}

rule "terraform_required_providers" {
  enabled = true
}

rule "terraform_documented_variables" {
  enabled = true
}

rule "terraform_documented_outputs" {
  enabled = true
}
