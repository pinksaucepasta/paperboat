package machineevents

// DaemonTopics is the user daemon's view of the worker's machine/account stream.
var DaemonTopics = []Topic{TopicInventory, TopicMachineAccess, TopicMachineServices, TopicPeerNetwork, TopicRecipe}
