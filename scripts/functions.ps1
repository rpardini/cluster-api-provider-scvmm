function VMToJson($vm) {
  $vm | %{ @{Cloud = $_.Cloud.Name; Name = $_.Name; Status = "$($_.Status)"; Memory = $_.Memory; CpuCount = $_.CpuCount; VirtualNetwork = $_.VirtualNetworkAdapters.VMNetwork.Name } } | convertto-json
}

function GetVM($vmname) {
  VMToJson((Get-SCVirtualMachine -Name $vmname))
}

function CreateVM($cloud, $vmname, $memory, $cpucount, $disksize, $vmnetwork) {
  $JobGroupID = [GUID]::NewGuid().ToString()
  New-SCVirtualDiskDrive -SCSI -Bus 0 -LUN 0 -JobGroup $JobGroupID -VirtualHardDiskSizeMB ([int]$disksize * 1024) -CreateDiffDisk $false -Dynamic -Filename "$($vmname)_disk_1" -VolumeType BootAndSystem

  $HardwareProfile = Get-SCHardwareProfile | where {$_.Name -eq "Server Gen 2 - Medium" }
  $LinuxOS = Get-SCOperatingSystem | where {$_.name -eq 'Other Linux (64 bit)'}

  $VMNetwork = Get-SCVMNetwork -Name $vmnetwork
  $VMSubnet = $VMNetwork.VMSubnet

  Set-SCVirtualNetworkAdapter -JobGroup $JobGroupID -SlotID 0 -VMNetwork $VMNetwork -VMSubnet $VMSubnet
  $virtualMachineConfiguration = New-SCVMConfiguration -VMTemplate $template -Name $vmname -VMHostGroup 'SO'
  $SCCloud = Get-SCCloud -Name $cloud
  New-SCVirtualMachine -Name $vmname -VMConfiguration $virtualMachineConfiguration -Cloud $SCCloud -Description "SO||talostest||manual" -JobGroup $JobGroupID -StartAction "NeverAutoTurnOnVM" -StopAction "SaveVM" -DynamicMemoryEnabled $false -MemoryMB [int]$memory -CPUCount [int]$cpucount -ReturnImmediately

  Get-SCVirtualMachine -name $vmname
}

function ReconcileVM($cloud, $vmname, $memory, $cpucount, $disksize, $vmnetwork) {
  $vm = Get-SCVirtualMachine -Name $vmname
  if (-not $vm) {
    $vm = CreateVM -cloud $cloud -vmname $vmname -memory $memory -cpucount $cpucount -disksize $disksize -vmnetwork $vmnetwork
  }
  VMToJson($vm)
}
