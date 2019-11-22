# -*- mode: ruby -*-
# vi: set ft=ruby :

Vagrant.configure("2") do |config|
  config.vm.box = "ubuntu/bionic64"

  # Sync the repo into the home folder of the box
  config.vm.synced_folder ".", "/home/vagrant/gazette"

  # Increase the default memory allocated to the VM
  config.vm.provider "virtualbox" do |vb|
    vb.memory = "4096"
  end

  # Install build-essential so that we can immediately run `make`
  config.vm.provision "shell", inline: <<-SHELL
    apt-get update
    apt-get install -y build-essential
  SHELL
end
