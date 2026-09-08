# Compare Go contract fixtures with the OneKS compiler in the supplied checkout.
# Usage: ruby hack/check-oneks-canonical.rb /path/to/one-ee
require 'json'
require 'digest'
require 'base64'
require File.join(ARGV.fetch(0), 'src/oneks/app/models/application_plan')

fixtures = JSON.parse(File.read(File.join(__dir__, '../internal/application/testdata/canonical-plans.json')))
fixtures.each do |fixture|
    spec = fixture.fetch('spec')
    input = OneKS::ApplicationPlan.canonical_plan_input(spec)
    input.delete('planDigest')
    unless OneKS::ApplicationPlan.canonical_json(input) == fixture.fetch('canonical') &&
           OneKS::ApplicationPlan.plan_digest(spec) == fixture.fetch('digest')
        abort "OneKS canonical contract differs: #{fixture.fetch('name')}"
    end
end
puts "OneKS canonical contract: #{fixtures.length} fixtures passed"
