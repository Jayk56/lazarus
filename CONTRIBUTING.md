# Contributing to Lazarus

Start with an issue for changes that affect the API, stored data, deployment
model, or maintenance lifecycle. Security reports belong in the private process
described in [SECURITY.md](SECURITY.md).

## Local checks

Lazarus requires Go, Helm, Python 3, Ansible, ShellCheck, Actionlint, and the
Python packages listed in `scripts/validation-requirements.txt`. Run the same
source gate as continuous integration:

```sh
python3 -m pip install -r scripts/validation-requirements.txt
make validate-source
```

The AAP configuration roles depend on collections supplied through Red Hat
Automation Hub. In an approved AAP execution environment, install
`examples/aap/collections/requirements.yml` and run `make aap-syntax-check`.

Keep changes focused, include tests for changed behavior, and update the API or
operator documentation when its contract changes. Pull requests should explain
the user-visible result and the validation that was run.

By submitting a contribution, you agree that it is licensed under the
[Apache License 2.0](LICENSE).
