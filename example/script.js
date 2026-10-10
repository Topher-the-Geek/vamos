const fs = require("fs");
console.log("Built at: " + fs.readFileSync("/home/node/build-date.txt", "utf8"));
